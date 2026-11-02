// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// What things cost, agreed by the network instead of a peg.
///
/// The coin floats. Fees are set in a reference basket of real things (its
/// contents are a published spec, chosen by governance: "1 kWh + 1 GB-month of
/// storage + 1 hour of a small server", say), so a message costs about the same
/// real work whatever the coin does on markets. Three parts:
///
///   1. The basket's price in coins: each round, anyone who bonds coins may
///      commit a sealed price and reveal it later; the bond-weighted median of
///      the reveals wins, but moves at most maxStepBps per round and per day
///      (from the price at the start of the oracle's day). Honest votes are
///      the Schelling point: a revealed vote within maxStepBps of the median
///      gets its bond back plus a share of what far votes lose; a vote outside
///      that band loses farSlashBps of its bond to the near voters. Pushing
///      the price costs the pusher, unless it holds the median; and then it
///      moves one step a day, while everyone sees the push coming.
///   2. Targets: what each kind of action should cost, in millionths of a
///      basket (governance sets them: "an envelope costs 10 ppm of a basket").
///   3. Congestion (EIP-1559): a multiplier that rises when blocks carry more
///      than the target load and decays back to 1 when they carry less.
///
/// fee(kind, n) = n × target[kind] × basketPrice / 1e6 × multiplier
///
/// Every number above is governed (the timelock: both houses): setParams for
/// the bond, step, quorum, slashing and round size, setRoundLength for the
/// rounds (a new length starts with the next round, so a round in progress
/// keeps its phases). And the oracle itself is replaceable: the Inbox prices
/// envelopes with the Directory's current "FeeOracle".
///
/// Bonds come back by pull (withdraw) once the round is finalized: a voter that
/// can't take coins can't block an eviction or a finalize. Rounds with votes
/// are finalized in order (an older round can't be skipped to drop its median).
contract FeeOracle {
    struct Vote {
        address voter;
        uint96 bond;
        bytes32 seal; // keccak256(abi.encode(price, salt, voter))
        uint256 price; // set on reveal
        bool revealed;
    }

    address public immutable governance; // the timelock: sets targets and the basket spec
    address payable public immutable treasury; // unrevealed bonds go here

    uint256 public basketPrice; // coin-wei per basket
    uint64 public lastFinalized; // the newest finalized round + 1
    mapping(uint64 => bool) public finalized;
    bytes32 public basketSpec; // hash of the published basket definition
    string public basketURI;
    mapping(bytes32 => uint256) public target; // kind => ppm of a basket per unit
    mapping(uint64 => Vote[]) internal votes;
    mapping(uint64 => mapping(address => uint256)) internal slot; // voter => index + 1
    mapping(address => uint256) public owed; // bonds to take back (withdraw)
    uint64 public constant DAY = 1 days;
    uint64 public dayStart; // the oracle's current day: the price moves at most maxStepBps from dayPrice in it
    uint256 public dayPrice;
    uint64[] internal voted; // rounds with commits, in order
    uint256 internal nextVoted; // the oldest of them not finalized

    // ---- congestion, per block (EIP-1559); one storage slot
    struct Load {
        uint128 multiplier; // 1e18 = 1×
        uint64 lastBlock;
        uint64 used; // units in lastBlock
    }

    Load internal load = Load(1e18, 0, 0);
    uint256 public targetPerBlock; // units of load a block should carry
    uint256 public maxMultiplier; // 1e18 = 1×
    mapping(address => bool) public meters; // contracts that report load (the Inbox)
    mapping(address => uint64) public slashedIn; // voter => the last round it was slashed in, + 1 (the slots above are fixed, see feeOracleLoadSlot)

    // ---- governed parameters (declared after `load`: its slot stays fixed)
    uint256 public minBond;
    uint256 public quorum; // revealed bond needed for the price to move
    uint16 public maxStepBps; // per round, e.g. 1000 = ±10% (the default)
    uint16 public maxVotes = 64; // per round; a bigger bond evicts the smallest
    uint16 public farSlashBps = 5000; // what a vote outside the band loses, to the near voters
    /// Slash caps (audit 5 IF-2). Per round: far votes together lose at most
    /// roundSlashBps of the round's revealed bond, scaled down pro rata
    /// (a split round is a disagreement, not a lie: a circuit breaker). Per
    /// reporter: a voter slashed in a round isn't slashed again in the next
    /// slashCooldown rounds (its bond comes back whole, without a share), so
    /// an honest minority reporting a real move against a stale majority
    /// loses one slash, not one a round. A lone far vote still loses farSlashBps.
    uint16 public roundSlashBps = 2000;
    uint64 public slashCooldown = 3;

    /// Rounds: round = base + (now - start) / length from `start` on. A new
    /// length (setRoundLength) is `next`, starting where the current round ends.
    struct Schedule {
        uint64 start;
        uint64 length;
        uint64 base;
    }

    Schedule internal sched;
    Schedule internal next;

    event Committed(uint64 indexed round, address indexed voter, uint256 bond);
    event Evicted(uint64 indexed round, address indexed voter);
    event Revealed(uint64 indexed round, address indexed voter, uint256 price, uint256 bond);
    event Finalized(uint64 indexed round, uint256 median, uint256 price, uint256 revealedBond);
    event Settled(uint64 indexed round, address indexed voter, uint256 back, bool near);
    event TargetSet(bytes32 indexed kind, uint256 ppm);
    event BasketSet(bytes32 spec, string uri);
    event MeterSet(address indexed meter, bool on);
    event ParamsSet(uint256 minBond, uint16 maxStepBps, uint256 quorum, uint16 farSlashBps, uint16 roundSlashBps, uint64 slashCooldown,
        uint16 maxVotes);
    event RoundLengthSet(uint64 length, uint64 from, uint64 firstRound);

    /// The launch values come in the constructor (targets, basket, meters), so
    /// nobody keeps an admin key: every later change is governance's.
    constructor(uint256 _basketPrice, uint64 _roundLength, uint256 _minBond, uint16 _maxStepBps, uint256 _quorum,
        address _governance, address payable _treasury, uint256 _targetPerBlock, uint256 _maxMultiplier,
        bytes32[] memory kinds, uint256[] memory ppms, bytes32 spec, string memory uri, address[] memory _meters) {
        require(_basketPrice > 0 && _roundLength >= 2 && _maxStepBps > 0 && _maxStepBps < 10000, "params");
        require(_maxMultiplier >= 1e18 && _maxMultiplier <= type(uint128).max && kinds.length == ppms.length, "params");
        require(_governance != address(0) && _treasury != address(0), "params");
        basketPrice = _basketPrice;
        sched = Schedule(uint64(block.timestamp), _roundLength, 0);
        dayStart = uint64(block.timestamp);
        dayPrice = _basketPrice;
        minBond = _minBond;
        maxStepBps = _maxStepBps;
        quorum = _quorum;
        governance = _governance;
        treasury = _treasury;
        targetPerBlock = _targetPerBlock;
        maxMultiplier = _maxMultiplier;
        for (uint256 i = 0; i < kinds.length; i++) _setTarget(kinds[i], ppms[i]);
        _setBasket(spec, uri);
        for (uint256 i = 0; i < _meters.length; i++) _setMeter(_meters[i], true);
    }

    modifier onlyGovernance() {
        require(msg.sender == governance, "governance only");
        _;
    }

    // ---------------------------------------------------------------- prices

    function _sched() internal view returns (Schedule memory s) {
        s = sched;
        if (next.start != 0 && block.timestamp >= next.start) s = next;
    }

    /// When the current schedule's rounds began (a round boundary).
    function start() external view returns (uint64) {
        return _sched().start;
    }

    /// Seconds per round now: commit in the first half, reveal in the second.
    function roundLength() external view returns (uint64) {
        return _sched().length;
    }

    function round() public view returns (uint64) {
        Schedule memory s = _sched();
        return s.base + (uint64(block.timestamp) - s.start) / s.length;
    }

    /// Seconds into the current round, and its length.
    function _phase() internal view returns (uint64 off, uint64 len) {
        Schedule memory s = _sched();
        return ((uint64(block.timestamp) - s.start) % s.length, s.length);
    }

    function revealing(uint64 r) public view returns (bool) {
        (uint64 off, uint64 len) = _phase();
        return round() == r && off >= len / 2;
    }

    /// The current price of `n` units of `kind`, in coin-wei, rounded up. Zero for kinds without a target.
    function fee(bytes32 kind, uint256 n) public view returns (uint256) {
        return _fee(kind, n, currentMultiplier());
    }

    function _fee(bytes32 kind, uint256 n, uint256 m) internal view returns (uint256) {
        uint256 x = n * target[kind] * basketPrice * m; // one division: no precision lost in between
        return x == 0 ? 0 : (x - 1) / 1e24 + 1;
    }

    /// Commits a sealed basket price for the current round: keccak256(abi.encode(price, salt, msg.sender)).
    function commit(bytes32 seal) external payable {
        uint64 r = round();
        require(!revealing(r), "commit phase is over");
        require(msg.value >= minBond && msg.value <= type(uint96).max, "bond");
        require(slot[r][msg.sender] == 0, "already committed");
        Vote[] storage vs = votes[r];
        if (voted.length == 0 || voted[voted.length - 1] != r) voted.push(r);
        if (vs.length < maxVotes) {
            vs.push(Vote(msg.sender, uint96(msg.value), seal, 0, false));
            slot[r][msg.sender] = vs.length;
        } else {
            uint256 low;
            for (uint256 i = 1; i < vs.length; i++) if (vs[i].bond < vs[low].bond) low = i;
            require(msg.value > vs[low].bond, "round full: bond more than the smallest");
            // FO-3: no evictions in the last quarter of the commit phase (evictees must have time to recommit)
            (uint64 off, uint64 len) = _phase();
            require(off < len * 3 / 8, "round full: evictions closed");
            Vote memory out = vs[low];
            delete slot[r][out.voter];
            vs[low] = Vote(msg.sender, uint96(msg.value), seal, 0, false);
            slot[r][msg.sender] = low + 1;
            owed[out.voter] += out.bond; // pulled later: a voter that rejects coins can't block the round
            emit Evicted(r, out.voter);
        }
        emit Committed(r, msg.sender, msg.value);
    }

    /// Reveals in the second half of the round; the bond is settled at finalize.
    function reveal(uint256 price, bytes32 salt) external {
        uint64 r = round();
        require(revealing(r), "not the reveal phase");
        uint256 s = slot[r][msg.sender];
        require(s != 0, "no commitment");
        Vote storage v = votes[r][s - 1];
        require(!v.revealed, "revealed");
        require(keccak256(abi.encode(price, salt, msg.sender)) == v.seal, "does not match the commitment");
        require(price > 0 && price <= type(uint128).max, "price");
        v.price = price;
        v.revealed = true;
        emit Revealed(r, msg.sender, price, v.bond);
    }

    /// Takes back bonds (settled or evicted); anyone may push the treasury's share to it.
    function withdraw(address payable to) external {
        uint256 a = owed[to];
        require(a > 0, "nothing owed");
        owed[to] = 0;
        (bool ok,) = to.call{value: a}("");
        require(ok, "send");
    }

    /// After a round ends, anyone may finalize it: the bond-weighted (lower)
    /// median of the reveals, moved toward by at most maxStepBps (per round and
    /// per day). Unrevealed bonds go to the treasury (a sealed vote that never
    /// opens only blocks others). With quorum, revealed votes outside
    /// median ± maxStepBps lose farSlashBps of their bond to the votes inside,
    /// pro rata; without quorum every revealed bond comes back whole.
    /// Rounds with votes are finalized oldest first (FO-2).
    function finalize(uint64 r) external {
        require(r < round(), "round not over");
        require(!finalized[r], "finalized");
        finalized[r] = true;
        if (r + 1 > lastFinalized) lastFinalized = r + 1;
        Vote[] storage vs = votes[r];
        uint256 len = vs.length;
        if (len > 0) {
            require(voted[nextVoted] == r, "finalize the older round with votes first");
            nextVoted++;
        }
        uint256[] memory p = new uint256[](len);
        uint256[] memory w = new uint256[](len);
        uint256 n;
        uint256 total;
        uint256 forfeit;
        for (uint256 i = 0; i < len; i++) {
            Vote storage v = vs[i];
            if (!v.revealed) {
                forfeit += v.bond;
                continue;
            }
            total += v.bond;
            // insertion sort by price (at most maxVotes)
            uint256 j = n++;
            while (j > 0 && p[j - 1] > v.price) {
                p[j] = p[j - 1];
                w[j] = w[j - 1];
                j--;
            }
            p[j] = v.price;
            w[j] = v.bond;
        }
        uint256 median;
        uint256 prev = basketPrice; // the price the round started from
        if (n > 0 && total >= quorum) {
            uint256 acc;
            for (uint256 i = 0; i < n; i++) {
                acc += w[i];
                if (acc * 2 >= total) {
                    median = p[i];
                    break;
                }
            }
            if (block.timestamp >= dayStart + DAY) {
                dayStart = uint64(block.timestamp);
                dayPrice = basketPrice;
            }
            basketPrice = _within(_capped(basketPrice, median), dayPrice);
        }
        forfeit += _settle(r, vs, median, prev);
        delete votes[r];
        if (forfeit > 0) owed[treasury] += forfeit;
        emit Finalized(r, median, basketPrice, total);
    }

    /// Bonds back: near votes whole plus a share of what far votes lose; far votes
    /// lose farSlashBps, within the caps above. median 0 (no quorum): everyone
    /// revealed gets the bond back. "Near" is anywhere from the round's starting
    /// price to the median, widened by maxStepBps on each side: it holds both
    /// the median band and the old-price band (and what lies between), so only
    /// a vote outside both is slashed. A bond majority defines the median, but a
    /// vote at the price everyone saw before the round is never far (audit 4
    /// IF-2: a whale can't slash the honest minority).
    /// Whale pushes don't cost bond (decided, audit 5 IF-2): a big bond voting a
    /// new price holds the median, so it is near. Slashing it by its distance
    /// from the old price would slash an honest majority reporting a real move
    /// too; the push is instead rate-limited (maxStepBps a round and a day) and
    /// public a round ahead. Returns the rounding dust.
    function _settle(uint64 r, Vote[] storage vs, uint256 median, uint256 prev) internal returns (uint256 dust) {
        (uint256 lo, uint256 hi) = median < prev ? (median, prev) : (prev, median);
        lo -= lo * maxStepBps / 10000;
        hi += hi * maxStepBps / 10000;
        uint256 near;
        uint256 lost;
        uint256 total;
        uint256 len = vs.length;
        uint8[] memory kind = new uint8[](len); // 0 unrevealed, 1 near, 2 far (slashed), 3 far in its cooldown (whole)
        for (uint256 i = 0; i < len; i++) {
            Vote storage v = vs[i];
            if (!v.revealed) continue;
            total += v.bond;
            if (median == 0 || _near(v.price, lo, hi)) {
                kind[i] = 1;
                near += v.bond;
            } else if (slashedIn[v.voter] != 0 && r < slashedIn[v.voter] + slashCooldown) {
                kind[i] = 3;
            } else {
                kind[i] = 2;
                lost += uint256(v.bond) * farSlashBps / 10000;
            }
        }
        uint256 full = lost; // the uncapped slash: each far vote's cut scales by lost / full
        if (lost > total * roundSlashBps / 10000) lost = total * roundSlashBps / 10000;
        uint256 taken;
        for (uint256 i = 0; i < len; i++) { // far votes first: what they lose is what near votes share
            if (kind[i] < 2) continue;
            Vote storage v = vs[i];
            uint256 back = v.bond;
            if (kind[i] == 2 && lost > 0) {
                uint256 cut = uint256(v.bond) * farSlashBps / 10000 * lost / full;
                back -= cut;
                taken += cut;
                slashedIn[v.voter] = r + 1;
            }
            owed[v.voter] += back;
            emit Settled(r, v.voter, back, false);
        }
        uint256 shared;
        for (uint256 i = 0; i < len; i++) {
            if (kind[i] != 1) continue;
            Vote storage v = vs[i];
            uint256 share = taken * v.bond / near;
            shared += share;
            owed[v.voter] += v.bond + share;
            emit Settled(r, v.voter, v.bond + share, true);
        }
        return taken - shared;
    }

    function _near(uint256 price, uint256 lo, uint256 hi) internal pure returns (bool) {
        return price >= lo && price <= hi;
    }

    /// Keeps `x` within maxStepBps of the day's starting price `d`.
    function _within(uint256 x, uint256 d) internal view returns (uint256) {
        uint256 step = d * maxStepBps / 10000;
        if (step == 0) step = 1;
        if (x > d + step) return d + step;
        if (x + step < d) return d - step == 0 ? 1 : d - step;
        return x;
    }

    /// Moves `cur` toward `want` by at most maxStepBps, and by at least one wei
    /// (so a tiny price can still move), never to zero.
    function _capped(uint256 cur, uint256 want) internal view returns (uint256) {
        uint256 step = cur * maxStepBps / 10000;
        if (step == 0) step = 1;
        if (want > cur) return want - cur > step ? cur + step : want;
        if (cur - want > step) want = cur - step;
        return want == 0 ? 1 : want;
    }

    function votesOf(uint64 r) external view returns (Vote[] memory) {
        return votes[r];
    }

    // ---------------------------------------------------------------- congestion

    function multiplier() external view returns (uint256) {
        return load.multiplier;
    }

    /// The multiplier as it would be now, decayed over the empty blocks since the last load.
    function currentMultiplier() public view returns (uint256 m) {
        Load memory l = load;
        m = l.multiplier;
        if (block.number == l.lastBlock || targetPerBlock == 0) return m;
        m = _step(m, l.used);
        uint256 empty = block.number - l.lastBlock - 1;
        for (uint256 i = 0; i < empty && i < 64 && m > 1e18; i++) m = _step(m, 0);
    }

    /// A metered contract (the Inbox) reports `units` of load and gets the fee
    /// for them, at the multiplier this block started with.
    function use(bytes32 kind, uint256 units) external returns (uint256 cost) {
        require(meters[msg.sender], "not a meter");
        Load memory l = load;
        if (block.number != l.lastBlock) {
            l.multiplier = uint128(currentMultiplier());
            l.lastBlock = uint64(block.number);
            l.used = 0;
        }
        cost = _fee(kind, units, l.multiplier);
        uint256 u = uint256(l.used) + units;
        l.used = u > type(uint64).max ? type(uint64).max : uint64(u);
        load = l;
    }

    /// m × (1 ± |used − target| / target / 8), within [1×, max]. Like EIP-1559,
    /// one block moves it at most 12.5% up: a single huge batch can't spike everyone's fee.
    function _step(uint256 m, uint256 used) internal view returns (uint256) {
        uint256 t = targetPerBlock;
        if (used >= t) {
            uint256 over = used - t > t ? t : used - t;
            m = m + m * over / t / 8;
            return m > maxMultiplier ? maxMultiplier : m;
        }
        uint256 down = m * (t - used) / t / 8;
        return m - down < 1e18 ? 1e18 : m - down;
    }

    // ---------------------------------------------------------------- governance

    function setTarget(bytes32 kind, uint256 ppm) external onlyGovernance {
        _setTarget(kind, ppm);
    }

    function setBasket(bytes32 spec, string calldata uri) external onlyGovernance {
        _setBasket(spec, uri);
    }

    function setCongestion(uint256 perBlock, uint256 maxMult) external onlyGovernance {
        require(maxMult >= 1e18 && maxMult <= type(uint128).max, "max");
        targetPerBlock = perBlock;
        maxMultiplier = maxMult;
        if (load.multiplier > maxMult) load.multiplier = uint128(maxMult);
        if (perBlock == 0) load.multiplier = 1e18; // FO-4: no congestion pricing means no multiplier, not a frozen one
    }

    function setMeter(address m, bool on) external onlyGovernance {
        _setMeter(m, on);
    }

    /// The bond, the step, the quorum and the slashing. Floors: a step within
    /// (0, 100%), shares in bps, at least one and at most 256 votes a round
    /// (finalize sorts them in one transaction).
    function setParams(uint256 minBond_, uint16 maxStepBps_, uint256 quorum_, uint16 farSlashBps_, uint16 roundSlashBps_,
        uint64 slashCooldown_, uint16 maxVotes_) external onlyGovernance {
        require(maxStepBps_ > 0 && maxStepBps_ < 10000 && farSlashBps_ <= 10000 && roundSlashBps_ <= 10000, "params");
        require(maxVotes_ > 0 && maxVotes_ <= 256, "votes per round: 1 to 256");
        // A cooldown near 2^64 rounds would overflow finalize and freeze the price (audit 6 FO-1).
        require(slashCooldown_ <= 10000, "slash cooldown: at most 10,000 rounds");
        minBond = minBond_;
        maxStepBps = maxStepBps_;
        quorum = quorum_;
        farSlashBps = farSlashBps_;
        roundSlashBps = roundSlashBps_;
        slashCooldown = slashCooldown_;
        maxVotes = maxVotes_;
        emit ParamsSet(minBond_, maxStepBps_, quorum_, farSlashBps_, roundSlashBps_, slashCooldown_, maxVotes_);
    }

    /// A new round length, from the end of the current round (it keeps its
    /// phases; round numbers go on counting up).
    function setRoundLength(uint64 length) external onlyGovernance {
        // A huge length couldn't be undone: it applies from the next round, which would never come (audit 6 FO-2).
        require(length >= 2 && length <= 30 days, "length: 2 s to 30 days");
        if (next.start != 0 && block.timestamp >= next.start) sched = next;
        Schedule memory s = sched;
        uint64 r = round();
        uint64 from = s.start + (r - s.base + 1) * s.length;
        next = Schedule(from, length, r + 1);
        emit RoundLengthSet(length, from, r + 1);
    }

    function _setTarget(bytes32 kind, uint256 ppm) internal {
        require(ppm <= 1e12, "ppm"); // up to a million baskets per unit
        target[kind] = ppm;
        emit TargetSet(kind, ppm);
    }

    function _setBasket(bytes32 spec, string memory uri) internal {
        basketSpec = spec;
        basketURI = uri;
        emit BasketSet(spec, uri);
    }

    function _setMeter(address m, bool on) internal {
        meters[m] = on;
        emit MeterSet(m, on);
    }
}
