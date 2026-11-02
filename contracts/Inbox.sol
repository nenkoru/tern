// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

interface IFeeBank {
    function collectFee(bytes32 from, address actor, uint256 amount, address payable to) external;
}

interface IDirectoryCurrent {
    function current(string calldata key) external view returns (address);
}

interface IFeeOracle {
    function fee(bytes32 kind, uint256 n) external view returns (uint256);
    function use(bytes32 kind, uint256 units) external returns (uint256);
}

/// The mailbox of the whole network: batches of sealed envelopes. Outsiders
/// see that envelopes exist, their size, their time and an 8-byte tag. The
/// fee makes mass mail cost money; it goes to whoever runs the network. It is
/// set by the network's FeeOracle (a basket of real things, priced by bonded
/// votes, times a congestion multiplier), never below minFee. Overpayment comes
/// back, so clients add headroom. Which FeeOracle is the Directory's current
/// "FeeOracle": both houses replace the pricing mechanism by publishing a new
/// version there (gov propose upgrade), with no new Inbox. An oracle that
/// fails (reverts, or doesn't list this Inbox as a meter) charges minFee.
///
/// Two ways to post a batch:
///   post / postPaid   the batch itself, in calldata: costly, and nobody can stop it
///   postRoot          only the batch's hash; the bytes are kept by the poster's
///                     data node (a relayer that stakes and names its URL).
///                     Anyone may challenge a hash whose data they can't get; the
///                     poster must then put the batch on chain in time, or lose
///                     its stake to the challenger (as OP Stack's Alt-DA challenges).
///
/// A hash's fee can't be forged (IN-1): postRoot charges for
/// max(n, ceil(size / ROOT_BYTES_PER_FEE)) envelopes and records the paid n and
/// size (paid(root)). Readers take a batch only if its length equals the paid
/// size, and deliver only its first n envelopes. A batch whose real count or
/// length differs from what was paid can't be resolved; anyone holding it proves
/// the fraud (proveFraud) while the poster's stake is locked and takes the slash.
///
/// Receipts (preconfirmations): a data node that accepts an envelope signs
/// {envelope hash, node, deadline, claim} (receiptDigest), where claim =
/// keccak(payee, salt) is chosen by the sender and only the sender knows its
/// preimage (audit 5 IP-1: the node can't claim the sender's receipt, and a
/// front-runner of the claim still pays the sender). It commits its batches with
/// postRootL, which also records the Merkle root of the batch's envelope hashes
/// (leaves(root); a batch whose envelopes don't match it is fraud, as above).
/// A receipt whose envelope is in no batch of that node committed by the
/// deadline: its holder calls proveMissed with a bond; the node rebuts within
/// resolveWindow with the batch root, a Merkle proof and the envelope itself,
/// which goes on chain (rebutMissed; IP-2: a root nobody can open proves
/// nothing, so the rebuttal delivers the envelope; the bond goes to the
/// rebutter), or anyone calls slashMissed: the payee gets its bond back and a
/// fixed bounty (challengeBond), the rest of the slash is burned (kept here for
/// good), so a node claiming receipts it signed to itself still loses its stake.
/// Unstaking takes notice (exitDA, then challengeWindow + resolveWindow), so a
/// node can't take its stake out under open receipts.
contract Inbox {
    uint256 public immutable minFee;   // per envelope, in the network coin: the floor
    address payable public immutable treasury;
    uint256 public count;
    IFeeBank public bank; // set once at deployment: fees can be paid from a name's Bank balance
    IDirectoryCurrent public immutable directory; // its "FeeOracle" prices envelopes (0: minFee only)
    bytes32 public constant ENVELOPE = keccak256("envelope");

    // ---- data availability
    uint256 public immutable minStake;      // a poster of hashes keeps this staked
    uint256 public immutable challengeBond; // a challenger puts this up
    uint64 public immutable challengeWindow;
    uint64 public immutable resolveWindow;
    mapping(address => uint256) public stake;
    mapping(address => uint64) public lockedUntil; // stake stays until its commitments can't be challenged
    // root => poster (160 bits) | paid n (24) | paid size (24) | committed at (40) | state (8):
    // 1 committed, 2 challenged, 3 resolved, 4 lost (or proven fraudulent)
    mapping(bytes32 => uint256) internal commits;
    uint256 public constant ROOT_BYTES_PER_FEE = 4096; // a hash pays one envelope's fee per 4 KiB at least
    mapping(address => uint256) public openChallenges; // a poster can't unstake while any is open (IN-2)
    mapping(bytes32 => address) public challenger;
    mapping(bytes32 => uint64) public challengedAt;
    mapping(bytes32 => bytes32) public leaves; // root => Merkle root of its envelopes' hashes (postRootL)
    mapping(address => uint64) public exitAt;  // a node may unstake after this (exitDA); 0: staying
    // keccak(envelope, node, claim) => payee (160) | claimed at (40) | deadline (40) | state (8): 1 open, 2 rebutted, 3 slashed
    mapping(bytes32 => uint256) internal misses;

    event Staked(address indexed poster, uint256 total, string url);
    event Committed(uint256 indexed first, uint256 n, bytes32 indexed root, uint256 size, address indexed poster);
    event Challenged(bytes32 indexed root, address indexed challenger);
    event Resolved(bytes32 indexed root, bytes batch);
    event Lost(bytes32 indexed root, address indexed poster);
    event Fraud(bytes32 indexed root, address indexed poster, uint256 count, uint256 size);
    event Missed(bytes32 indexed env, address indexed node, uint64 deadline, bytes32 claim, address claimant);
    event Rebutted(bytes32 indexed env, address indexed node, bytes32 root);
    event MissSlashed(bytes32 indexed env, address indexed node, uint256 slash);
    event Exiting(address indexed node, uint64 at);
    address private immutable deployer;

    event Posted(uint256 indexed first, uint256 n, bytes batch);

    constructor(uint256 _minFee, address payable _treasury, uint256 _minStake, uint256 _challengeBond, uint64 _challengeWindow,
        uint64 _resolveWindow, IDirectoryCurrent _directory) {
        require(_minFee > 0, "a fee floor above zero");
        minFee = _minFee;
        treasury = _treasury;
        minStake = _minStake;
        challengeBond = _challengeBond;
        challengeWindow = _challengeWindow;
        resolveWindow = _resolveWindow;
        directory = _directory;
        deployer = msg.sender;
    }

    function setBank(IFeeBank b) external {
        require(msg.sender == deployer && address(bank) == address(0), "once, at deployment");
        bank = b;
    }

    /// The FeeOracle in force: the Directory's current one.
    function oracle() public view returns (IFeeOracle) {
        if (address(directory) == address(0)) return IFeeOracle(address(0));
        try directory.current("FeeOracle") returns (address o) {
            return IFeeOracle(o);
        } catch {
            return IFeeOracle(address(0));
        }
    }

    /// What one envelope costs now (clients quote n × fee() plus headroom).
    function fee() public view returns (uint256) {
        IFeeOracle o = oracle();
        if (address(o).code.length == 0) return minFee;
        try o.fee(ENVELOPE, 1) returns (uint256 f) {
            return f > minFee ? f : minFee;
        } catch {
            return minFee;
        }
    }

    /// Charges n envelopes: the oracle's price, which also counts the load, or the floor.
    function charge(uint256 n) internal returns (uint256 total) {
        total = minFee * n;
        IFeeOracle o = oracle();
        if (address(o).code.length == 0) return total;
        try o.use(ENVELOPE, n) returns (uint256 f) {
            if (f > total) total = f;
        } catch {}
    }

    /// The fee to the treasury, the rest back to the sender.
    function settle(uint256 total) internal {
        require(msg.value >= total, "fee");
        (bool ok,) = treasury.call{value: total}("");
        require(ok, "treasury");
        if (msg.value > total) {
            (ok,) = payable(msg.sender).call{value: msg.value - total}("");
            require(ok, "refund");
        }
    }

    /// Counts envelopes: "TC2" | epoch(8) | eph(32) | tag(8) | tag2(8) | len(4) | ct(len).
    function countEnvelopes(bytes calldata batch) public pure returns (uint256 n) {
        uint256 i = 0;
        while (i < batch.length) {
            require(i + 63 <= batch.length && batch[i] == "T" && batch[i + 1] == "C" && batch[i + 2] == "2", "bad envelope");
            uint256 len = uint256(uint32(bytes4(batch[i + 59:i + 63])));
            i += 63 + len;
            require(i <= batch.length, "bad envelope length");
            n++;
        }
    }

    /// The fee paid from a name's Bank balance instead of attached coins: an
    /// agent's fees count against its spend limit, and it needs no coins of its
    /// own. The paying name is visible to the Bank; use a relayer to hide it.
    /// Its ceiling: POSTPAID_X times the floor fee per envelope (audit 6
    /// FO-3: an oracle a vote installed can't drain a name; postPaidUpTo sets one's own).
    uint256 public constant POSTPAID_X = 1000;

    function postPaid(bytes32 from, bytes calldata batch, uint256 n) external {
        postPaidUpTo(from, batch, n, minFee * n * POSTPAID_X);
    }

    /// postPaid with a ceiling (IN-3): reverts if the fee at inclusion is above maxFee.
    function postPaidUpTo(bytes32 from, bytes calldata batch, uint256 n, uint256 maxFee) public {
        check(batch, n);
        uint256 total = charge(n);
        require(total <= maxFee, "fee above the sender's ceiling");
        bank.collectFee(from, msg.sender, total, treasury);
        uint256 first = count;
        count = first + n;
        emit Posted(first, n, batch);
    }

    function check(bytes calldata batch, uint256 n) internal pure {
        require(n > 0 && n <= 256, "1..256 envelopes");
        require(batch.length <= 128 * 1024, "batch too big");
        require(countEnvelopes(batch) == n, "envelope count does not match the batch");
    }

    function post(bytes calldata batch, uint256 n) external payable {
        check(batch, n);
        uint256 total = charge(n);
        uint256 first = count;
        count = first + n;
        emit Posted(first, n, batch);
        settle(total);
    }

    // ---- data availability: hashes on chain, bytes at data nodes

    /// A data node stakes and says where its batches can be fetched (GET url/batch/<root>).
    function stakeDA(string calldata url) external payable {
        stake[msg.sender] += msg.value;
        exitAt[msg.sender] = 0;
        emit Staked(msg.sender, stake[msg.sender], url);
    }

    /// Notice before unstaking: receipts the node signed can still be claimed.
    function exitDA() external {
        uint64 at = uint64(block.timestamp) + challengeWindow + resolveWindow;
        exitAt[msg.sender] = at;
        emit Exiting(msg.sender, at);
    }

    function unstakeDA(uint256 amount) external {
        require(block.timestamp > lockedUntil[msg.sender], "commitments can still be challenged");
        require(openChallenges[msg.sender] == 0, "a challenge is open");
        uint64 e = exitAt[msg.sender];
        require(e != 0 && block.timestamp > e, "give notice first (exitDA): receipts can still be claimed");
        require(stake[msg.sender] >= amount, "stake");
        stake[msg.sender] -= amount;
        (bool ok,) = payable(msg.sender).call{value: amount}("");
        require(ok, "send");
    }

    /// Envelopes a hash pays for: n, or one per ROOT_BYTES_PER_FEE of its size if more.
    function rootUnits(uint256 n, uint256 size) public pure returns (uint256) {
        uint256 bySize = (size + ROOT_BYTES_PER_FEE - 1) / ROOT_BYTES_PER_FEE;
        return bySize > n ? bySize : n;
    }

    /// What a hash costs now (clients quote it with headroom).
    function rootFee(uint256 n, uint256 size) external view returns (uint256) {
        return fee() * rootUnits(n, size);
    }

    /// Commits a batch by its hash: keccak256(batch). The fee is per envelope, as
    /// for post, and per 4 KiB of size if that is more; the paid n and size are
    /// recorded, and readers hold the batch to them.
    function postRoot(bytes32 root, uint256 n, uint256 size) external payable {
        _postRoot(root, n, size);
    }

    /// postRoot that also records the Merkle root of the envelopes' hashes
    /// (keccak256 of each envelope, in order; pairs hashed 0x01|left|right, an
    /// odd last node carried up; an envelope starts with "TC2", so a node is
    /// never a leaf, IP-8): what a node's receipts are proven against.
    function postRootL(bytes32 root, uint256 n, uint256 size, bytes32 leafRoot) external payable {
        require(leafRoot != 0, "leaf root");
        leaves[root] = leafRoot;
        _postRoot(root, n, size);
    }

    function _postRoot(bytes32 root, uint256 n, uint256 size) internal {
        require(n > 0 && n <= 4096 && size >= 63 * n && size <= 4 << 20, "batch size");
        require(stake[msg.sender] >= minStake, "stake first (stakeDA)");
        require(commits[root] == 0, "already committed");
        commits[root] = _pack(msg.sender, n, size, uint64(block.timestamp), 1);
        uint64 until = uint64(block.timestamp) + challengeWindow + resolveWindow;
        if (lockedUntil[msg.sender] < until) lockedUntil[msg.sender] = until;
        uint256 first = count;
        count = first + n;
        emit Committed(first, n, root, size, msg.sender);
        settle(charge(rootUnits(n, size)));
    }

    function _pack(address poster, uint256 n, uint256 size, uint64 at, uint8 state) internal pure returns (uint256) {
        return uint256(uint160(poster)) << 96 | n << 72 | size << 48 | uint256(uint40(at)) << 8 | state;
    }

    function _setState(bytes32 root, uint8 state) internal {
        commits[root] = commits[root] & ~uint256(0xff) | state;
    }

    function commitment(bytes32 root) public view returns (address poster, uint64 at, uint8 state) {
        uint256 c = commits[root];
        return (address(uint160(c >> 96)), uint64(uint40(c >> 8)), uint8(c));
    }

    /// What a committed hash paid for: readers deliver only the first n envelopes
    /// of a batch whose length is exactly size (IN-1).
    function paid(bytes32 root) external view returns (uint256 n, uint256 size) {
        uint256 c = commits[root];
        return (uint24(c >> 72), uint24(c >> 48));
    }

    /// The batch matches what its poster paid for: its length and envelope count.
    function matchesPaid(bytes32 root, bytes calldata batch) public view returns (bool) {
        uint256 c = commits[root];
        if (batch.length != uint24(c >> 48) || countEnvelopes(batch) != uint24(c >> 72)) return false;
        bytes32 l = leaves[root];
        return l == 0 || leafRootOf(batch) == l;
    }

    /// The Merkle root of a batch's envelope hashes (see postRootL).
    function leafRootOf(bytes calldata batch) public pure returns (bytes32) {
        bytes32[] memory h = new bytes32[](countEnvelopes(batch));
        uint256 i = 0;
        for (uint256 k = 0; k < h.length; k++) {
            uint256 j = i + 63 + uint256(uint32(bytes4(batch[i + 59:i + 63])));
            h[k] = keccak256(batch[i:j]);
            i = j;
        }
        uint256 n = h.length;
        if (n == 0) return 0;
        while (n > 1) {
            for (uint256 k = 0; k < n / 2; k++) h[k] = keccak256(abi.encodePacked(bytes1(0x01), h[2 * k], h[2 * k + 1]));
            if (n % 2 == 1) h[n / 2] = h[n - 1];
            n = (n + 1) / 2;
        }
        return h[0];
    }

    // ---- receipts: a node's promise to commit an envelope by a deadline

    function receiptDigest(bytes32 env, address node, uint64 deadline, bytes32 claim) public view returns (bytes32) {
        return keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32",
            keccak256(abi.encode("tern-receipt", address(this), block.chainid, env, node, deadline, claim))));
    }

    function missOf(bytes32 env, address node, bytes32 claim) public view returns (address payee, uint64 at, uint64 deadline, uint8 state) {
        uint256 m = misses[keccak256(abi.encode(env, node, claim))];
        return (address(uint160(m >> 96)), uint64(uint40(m >> 56)), uint64(uint40(m >> 16)), uint8(m));
    }

    /// "This node signed a receipt for env and didn't commit it by the deadline."
    /// payee and salt open the receipt's claim; the bounty goes to payee, whoever sends this.
    function proveMissed(bytes32 env, address node, uint64 deadline, address payee, bytes32 salt, bytes calldata sig) external payable {
        require(block.timestamp > deadline, "the receipt is not due yet");
        require(block.timestamp <= deadline + challengeWindow, "too late to claim");
        require(sig.length == 65 && payee != address(0), "signature");
        bytes32 claim = keccak256(abi.encode(payee, salt));
        bytes32 r = bytes32(sig[0:32]);
        bytes32 s = bytes32(sig[32:64]);
        uint8 v = uint8(sig[64]);
        if (v < 27) v += 27;
        require(ecrecover(receiptDigest(env, node, deadline, claim), v, r, s) == node && node != address(0), "not this node's receipt");
        require(stake[node] > 0, "not a staked node");
        bytes32 k = keccak256(abi.encode(env, node, claim));
        require(misses[k] == 0, "already claimed");
        require(msg.value == challengeBond, "bond");
        misses[k] = uint256(uint160(payee)) << 96 | uint256(uint40(block.timestamp)) << 56 | uint256(uint40(deadline)) << 16 | 1;
        openChallenges[node]++;
        emit Missed(env, node, deadline, claim, payee);
    }

    /// The node (or anyone) shows env is envelope `index` of a batch the node
    /// committed by the deadline and not lost, and puts the envelope itself on
    /// chain (Posted: readers take it from there, whatever the batch's fate, IP-2):
    /// the claim fails, its bond pays the rebutter.
    function rebutMissed(bytes32 env, address node, bytes32 claim, bytes32 root, uint256 index, bytes32[] calldata proof,
        bytes calldata envelope) external {
        bytes32 k = keccak256(abi.encode(env, node, claim));
        (, uint64 claimed, uint64 deadline, uint8 st) = missOf(env, node, claim);
        require(st == 1, "no open claim");
        require(block.timestamp <= claimed + resolveWindow, "too late to rebut");
        (address poster, uint64 at, uint8 state) = commitment(root);
        require(poster == node && at <= deadline && state != 0 && state != 4, "not a batch of this node, committed in time");
        require(keccak256(envelope) == env && countEnvelopes(envelope) == 1, "not this envelope");
        uint256 n = uint24(commits[root] >> 72);
        require(index < n, "index");
        bytes32 h = env;
        for (uint256 i = 0; n > 1; n = (n + 1) / 2) {
            if (index % 2 == 1) h = keccak256(abi.encodePacked(bytes1(0x01), proof[i++], h));
            else if (index + 1 < n) h = keccak256(abi.encodePacked(bytes1(0x01), h, proof[i++]));
            index /= 2;
        }
        require(h == leaves[root] && h != 0, "not in that batch");
        misses[k] = misses[k] & ~uint256(0xff) | 2;
        openChallenges[node]--;
        uint256 first = count;
        count = first + 1;
        emit Posted(first, 1, envelope);
        emit Rebutted(env, node, root);
        (bool ok,) = payable(msg.sender).call{value: challengeBond}("");
        require(ok, "send");
    }

    /// Not rebutted in time: the node's stake (up to minStake) is taken; the payee
    /// gets its bond back and a fixed bounty, and the rest is burned (stays in this
    /// contract, owed to no one): a node can't recover its own slash (IP-1).
    function slashMissed(bytes32 env, address node, bytes32 claim) external {
        bytes32 k = keccak256(abi.encode(env, node, claim));
        (address payee, uint64 claimed,, uint8 st) = missOf(env, node, claim);
        require(st == 1 && block.timestamp > claimed + resolveWindow, "no claim past its rebuttal window");
        misses[k] = misses[k] & ~uint256(0xff) | 3;
        openChallenges[node]--;
        uint256 slash = stake[node] < minStake ? stake[node] : minStake;
        stake[node] -= slash;
        uint256 bounty = slash < challengeBond ? slash : challengeBond;
        emit MissSlashed(env, node, slash);
        (bool ok,) = payable(payee).call{value: bounty + challengeBond}("");
        require(ok, "send");
    }

    /// "I can't get this batch": the poster must put it on chain within resolveWindow.
    function challenge(bytes32 root) external payable {
        (address poster, uint64 at, uint8 state) = commitment(root);
        require(state == 1, "nothing to challenge");
        require(block.timestamp <= at + challengeWindow, "too late to challenge");
        require(msg.value == challengeBond, "bond");
        _setState(root, 2);
        openChallenges[poster]++;
        challenger[root] = msg.sender;
        challengedAt[root] = uint64(block.timestamp);
        emit Challenged(root, msg.sender);
    }

    /// Anyone holding the batch puts it on chain; the challenger's bond pays for it.
    /// A batch that differs from what was paid for can't be resolved: proveFraud.
    function resolve(bytes32 root, bytes calldata batch) external {
        (address poster,, uint8 state) = commitment(root);
        require(state == 2, "not challenged");
        require(block.timestamp <= challengedAt[root] + resolveWindow, "too late");
        require(keccak256(batch) == root, "not this batch");
        require(matchesPaid(root, batch), "batch differs from what was paid for: proveFraud");
        _setState(root, 3);
        openChallenges[poster]--;
        emit Resolved(root, batch);
        (bool ok,) = payable(msg.sender).call{value: challengeBond}("");
        require(ok, "send");
    }

    /// Nobody put it on chain in time: the batch is lost; the poster's stake pays the challenger.
    function expire(bytes32 root) external {
        (address poster,, uint8 state) = commitment(root);
        require(state == 2 && block.timestamp > challengedAt[root] + resolveWindow, "not expired");
        _setState(root, 4);
        openChallenges[poster]--;
        uint256 slash = stake[poster] < minStake ? stake[poster] : minStake;
        stake[poster] -= slash;
        emit Lost(root, poster);
        (bool ok,) = payable(challenger[root]).call{value: slash + challengeBond}("");
        require(ok, "send");
    }

    /// A batch whose real envelope count or length differs from what its poster
    /// paid for (IN-1): anyone holding it proves it while the poster's stake is
    /// still locked, and takes the slash. The root counts as lost (readers drop it);
    /// an open challenge's bond goes back to its challenger. Readers that apply the
    /// paid rule never delivered the unpaid envelopes, so fraud gains nothing.
    function proveFraud(bytes32 root, bytes calldata batch) external {
        (address poster, uint64 at, uint8 state) = commitment(root);
        require(state == 1 || state == 2, "nothing to prove");
        require(block.timestamp <= at + challengeWindow + resolveWindow, "too late");
        require(keccak256(batch) == root, "not this batch");
        require(!matchesPaid(root, batch), "the batch is what was paid for");
        _setState(root, 4);
        uint256 slash = stake[poster] < minStake ? stake[poster] : minStake;
        stake[poster] -= slash;
        emit Fraud(root, poster, countEnvelopes(batch), batch.length);
        emit Lost(root, poster);
        if (state == 2) {
            openChallenges[poster]--;
            (bool ok0,) = payable(challenger[root]).call{value: challengeBond}("");
            require(ok0, "send");
        }
        // half to the prover, half to the treasury: proving your own fraud still costs you
        (bool ok,) = payable(msg.sender).call{value: slash - slash / 2}("");
        require(ok, "send");
        (ok,) = treasury.call{value: slash / 2}("");
        require(ok, "treasury");
    }
}
