// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

interface IValidatorRegistry {
    function canAct(bytes32 id, address who) external view returns (bool ok, bytes32 owner, bool isAgent);
}

interface IValidatorCitizens {
    function isCitizen(bytes32 id) external view returns (bool);
    function canVote(bytes32 id, uint64 at) external view returns (bool);
    function count() external view returns (uint32);
}

/// The validator set of a ternchain L1 (Hyperledger Besu, QBFT with
/// "validatorcontractaddress"). Besu calls getValidators() on the parent
/// block's state to know who may propose and seal each block.
///
/// The list changes only by the network's governance (the timelock, whose
/// only proposer is Parliament: both houses, then the exit window), and only
/// slowly (docs/abuse/VALIDATORS.md):
///   - a rolling cap: at most max(1, ⌊n0/3⌋) changes (adds + removes) within
///     `rules.window`, n0 being the set before that window's changes. A
///     faction can't replace a third of the set at once, nor by salami;
///   - a removal is scheduled, and takes effect `rules.removalDelay` later
///     (on top of the exit window): removals wait longer than additions;
///   - the set never drops below `rules.minSet`;
///   - ChurnAlert when the changes in the window pass 1/4 of n0.
/// One validator per citizen: each address is bound to a citizen's name, and
/// only with that citizen's consent (it offers the key; governance adds it).
/// A citizen rotates its own key; a vote naming an old key follows the
/// rotation to the citizen's current one. A citizen who lapses or is revoked
/// loses its validator (anyone calls lapse(); above the minimum set it is not
/// held back by the cap). Citizens challenge "same operator" pairs with
/// evidence and vote; a passed challenge is a finding, not a removal: the
/// removal is a Parliament vote (both houses, the exit window) citing it.
///
/// The contract lives in the genesis alloc (QBFT reads it from block 0, before
/// any transaction could deploy it), as runtime code plus storage computed off
/// chain (`ternchain validator genesis`; layout below, checked against this
/// constructor by validators_test.go). The genesis names a bootstrap address
/// (the deployer) whose one power is to name the governance once, after the
/// ternchain contracts are deployed; then it has none.
///
/// Storage layout (what genesis writes; the rest is zero):
///   slot 0: list.length;  keccak256(0) + i: list[i]
///   slot 1: position[a] = i + 1 at keccak256(abi.encode(a, 1))
///   slot 2: governance (0 at genesis)
///   slot 3: bootstrap
///   slot 4: upgrades.length (0 at genesis)
///   slot 5: rules.window | removalDelay << 64 | lapseGrace << 128 | minSet << 192
///   slot 6: rules.voteWindow | quorumBps << 64
///   slot 7: citizenOf[a] at keccak256(abi.encode(a, 7))
///   slot 8: validatorOf[c] at keccak256(abi.encode(c, 8))
/// (slots 9 and up start empty: genesis writes none of them.)
contract Validators {
    struct Upgrade {
        uint64 atBlock;     // the block the new rules start at
        bytes32 release;    // hash of the release (e.g. sha256 of the Besu tarball or genesis diff)
        string uri;         // where the release and its notes are
    }

    struct Rules {
        uint64 window;       // seconds the rolling cap counts changes over
        uint64 removalDelay; // seconds a scheduled removal waits (after the exit window)
        uint64 lapseGrace;   // seconds after the handover before a non-citizen's validator can lapse
        uint32 minSet;       // the set never drops below this
        uint64 voteWindow;   // seconds a same-operator challenge is voted on
        uint16 quorumBps;    // a challenge's turnout, as a share of citizens
    }

    /// Why a removal was scheduled.
    uint8 public constant BY_VOTE = 0;
    uint8 public constant LAPSED = 1;
    uint8 public constant SAME_OPERATOR = 2;
    uint64 private constant ADDED = 1 << 63; // the flag on a change: an addition (else a removal)

    struct Challenge {
        address a;           // kept
        address b;           // removed if the challenge passes
        bytes32 by;
        uint64 opened;
        uint64 ends;
        uint32 electorate;
        uint32 yes;
        uint32 no;
        bool closed;
        bool passed;
        bytes32 evidence;    // hash of the evidence (shared IP/ASN, key reuse, …)
    }

    address[] private list;
    mapping(address => uint256) private position;
    address public governance;
    address public bootstrap;
    Upgrade[] private upgrades;
    Rules public rules;
    mapping(address => bytes32) public citizenOf;
    mapping(bytes32 => address) public validatorOf;
    uint64[] private changes; // when each applied change happened (| ADDED for an addition)
    mapping(address => uint64) public removalAt; // a scheduled removal takes effect from (0: none)
    mapping(address => uint8) public removalWhy;
    IValidatorRegistry public registry;
    IValidatorCitizens public citizens;
    uint64 public governedSince;
    Challenge[] private challenges;
    mapping(uint256 => mapping(bytes32 => bool)) public challengeVoted;
    // A key rotated away from: its citizen, and that citizen's binding then
    // (a vote naming it reaches the citizen's current key; a removal ends the binding).
    mapping(address => bytes32) public formerCitizen;
    mapping(address => uint64) private formerBinding;
    mapping(bytes32 => uint64) private binding;
    mapping(bytes32 => address) public offered;              // the key a citizen consents to run (addValidator needs it)
    mapping(uint256 => bytes32[2]) private accused;          // a challenge's two citizens, fixed when it opens

    event ValidatorAdded(address indexed validator, bytes32 indexed citizen);
    event ValidatorRemoved(address indexed validator, bytes32 indexed citizen, uint8 why);
    event RemovalScheduled(address indexed validator, bytes32 indexed citizen, uint8 why, uint64 at);
    event RemovalCancelled(address indexed validator, bytes32 indexed citizen);
    event ValidatorRotated(bytes32 indexed citizen, address indexed from, address indexed to);
    event ValidatorOffered(bytes32 indexed citizen, address indexed validator);
    event ChurnAlert(uint256 changes, uint256 setBefore, uint64 window);
    event ChallengeOpened(uint256 indexed id, address a, address b, bytes32 indexed by, bytes32 evidence, string uri, uint64 ends);
    event ChallengeVoted(uint256 indexed id, bytes32 indexed by, bool yes);
    event ChallengeClosed(uint256 indexed id, bool passed, uint32 yes, uint32 no);
    event CensorshipReported(bytes32 indexed txHash, bytes32 indexed by, uint64 firstSeenBlock, uint64 blocksPending, address[] proposers,
        string uri);
    event RulesSet(Rules rules);
    event GovernanceSet(address indexed governance);
    event UpgradeScheduled(uint256 indexed id, uint64 atBlock, bytes32 release, string uri);

    /// For deployments outside genesis (tests); a genesis alloc writes the same storage.
    constructor(address bootstrap_, address[] memory initial, bytes32[] memory names, Rules memory r) {
        require(names.length == initial.length, "one name per validator (0: unbound)");
        bootstrap = bootstrap_;
        _setRules(r);
        for (uint256 i = 0; i < initial.length; i++) _add(initial[i], names[i]);
    }

    modifier onlyGovernance() {
        require(governance != address(0) && msg.sender == governance, "governance only");
        _;
    }

    // ---- what Besu reads

    function getValidators() external view returns (address[] memory) {
        return list;
    }

    function isValidator(address a) external view returns (bool) {
        return position[a] != 0;
    }

    function count() external view returns (uint256) {
        return list.length;
    }

    // ---- the handover

    /// Once, by the bootstrap: hand the set to the timelock, and name the
    /// Registry and the citizens' house it binds validators to. Then the
    /// bootstrap has no powers.
    function setGovernance(address g, address registry_, address citizens_) external {
        require(msg.sender == bootstrap && governance == address(0), "bootstrap only, once");
        require(g.code.length > 0 && registry_.code.length > 0 && citizens_.code.length > 0, "governance, registry, citizens: contracts");
        governance = g;
        registry = IValidatorRegistry(registry_);
        citizens = IValidatorCitizens(citizens_);
        governedSince = uint64(block.timestamp);
        bootstrap = address(0);
        emit GovernanceSet(g);
    }

    // ---- set changes by governance (one call per proposal: one change per proposal)

    /// A citizen's validator joins (at once, after the exit window), within
    /// the cap, and only the key that citizen offered (offerValidator).
    function addValidator(address v, bytes32 citizen) external onlyGovernance {
        require(citizen != 0 && citizens.isCitizen(citizen), "a validator is a citizen's");
        require(validatorOf[citizen] == address(0), "one validator per citizen");
        require(v != address(0) && offered[citizen] == v, "the citizen did not offer this key (gov validator offer)");
        delete offered[citizen];
        _capped();
        _add(v, citizen);
        _record(true);
    }

    /// A removal by vote: scheduled; it takes effect removalDelay later (applyRemoval).
    /// A key its citizen rotated away from is followed to the current one.
    function removeValidator(address v) external onlyGovernance {
        _schedule(_current(v), BY_VOTE);
    }

    /// A scheduled removal undone by another vote (within removalDelay).
    function cancelRemoval(address v) external onlyGovernance {
        v = _current(v);
        require(removalAt[v] != 0, "nothing scheduled");
        delete removalAt[v];
        delete removalWhy[v];
        emit RemovalCancelled(v, citizenOf[v]);
    }

    function setRules(Rules calldata r) external onlyGovernance {
        _setRules(r);
    }

    /// A chain-software upgrade: every validator must run `release` before
    /// `atBlock` (Besu activates forks at a block or time written in each
    /// node's genesis; this is the on-chain, governed schedule they follow).
    function scheduleUpgrade(uint64 atBlock, bytes32 release, string calldata uri) external onlyGovernance {
        require(atBlock > block.number, "in the past");
        upgrades.push(Upgrade(atBlock, release, uri));
        emit UpgradeScheduled(upgrades.length - 1, atBlock, release, uri);
    }

    // ---- anyone: the removals that are due

    /// A scheduled removal, once due: above the minimum set, and (but for a
    /// lapse: a non-citizen's node) within the cap; else it stays scheduled
    /// and can be applied later. A lapsed citizen who is a citizen again
    /// keeps its validator.
    function applyRemoval(address v) external {
        uint64 at = removalAt[v];
        require(at != 0 && block.timestamp >= at, "no removal due");
        bytes32 c = citizenOf[v];
        if (removalWhy[v] == LAPSED && c != 0 && citizens.isCitizen(c)) {
            delete removalAt[v];
            delete removalWhy[v];
            emit RemovalCancelled(v, c);
            return;
        }
        require(list.length > rules.minSet, "the minimum set: add a validator first");
        uint8 why = removalWhy[v];
        if (why != LAPSED) _capped();
        _remove(v);
        _record(false);
        emit ValidatorRemoved(v, c, why);
    }

    /// A validator whose citizen is no longer a citizen (revoked, lapsed,
    /// left): its removal is scheduled. After lapseGrace from the handover,
    /// so founders have time to claim.
    function lapse(address v) external {
        bytes32 c = citizenOf[v];
        require(c != 0, "unbound: removed only by vote");
        require(governedSince != 0 && block.timestamp >= uint256(governedSince) + rules.lapseGrace, "the grace after the handover");
        require(!citizens.isCitizen(c), "its citizen is a citizen");
        _schedule(v, LAPSED);
    }

    // ---- the citizen's own validator

    /// A citizen consents to run key v (0: withdraws): governance can then add it.
    function offerValidator(bytes32 citizen, address v) external {
        _citizenOwner(citizen);
        offered[citizen] = v;
        emit ValidatorOffered(citizen, v);
    }

    /// A citizen moves its validator to a new key (a stolen or lost node
    /// key): the old address stops counting from the next block.
    function rotate(bytes32 citizen, address to) external {
        _citizenOwner(citizen);
        address v = validatorOf[citizen];
        require(v != address(0), "no validator of yours");
        require(removalAt[v] == 0, "a removal is scheduled");
        require(to != address(0) && position[to] == 0 && citizenOf[to] == 0, "to: a new address");
        uint256 p = position[v];
        list[p - 1] = to;
        position[to] = p;
        delete position[v];
        delete citizenOf[v];
        formerCitizen[v] = citizen;
        formerBinding[v] = binding[citizen];
        citizenOf[to] = citizen;
        validatorOf[citizen] = to;
        emit ValidatorRotated(citizen, v, to);
    }

    // ---- same operator: citizens vote on evidence

    function challenge(bytes32 by, address a, address b, bytes32 evidence, string calldata uri) external returns (uint256 id) {
        _citizenOwner(by);
        require(a != b && position[a] != 0 && position[b] != 0, "two validators");
        require(evidence != 0 && bytes(uri).length > 0, "evidence: its hash and where to read it");
        id = challenges.length;
        uint64 ends = uint64(block.timestamp) + rules.voteWindow;
        challenges.push(Challenge(a, b, by, uint64(block.timestamp), ends, citizens.count(), 0, 0, false, false, evidence));
        accused[id] = [citizenOf[a], citizenOf[b]]; // a rotation later doesn't let them vote
        emit ChallengeOpened(id, a, b, by, evidence, uri, ends);
    }

    function voteChallenge(uint256 id, bytes32 by, bool yes) external {
        _owner(by);
        Challenge storage x = challenges[id];
        require(block.timestamp <= x.ends, "voting closed");
        require(citizens.canVote(by, x.opened), "citizens from before the challenge only");
        bytes32[2] storage acc = accused[id];
        require(by != acc[0] && by != acc[1] && by != citizenOf[x.a] && by != citizenOf[x.b], "the accused don't vote");
        require(!challengeVoted[id][by], "voted");
        challengeVoted[id][by] = true;
        if (yes) x.yes++;
        else x.no++;
        emit ChallengeVoted(id, by, yes);
    }

    /// After the window: 2/3 yes on a turnout of quorumBps (never below
    /// min(5, half the citizens)) is a finding. It removes no one: the removal
    /// is a Parliament proposal (removeValidator: both houses, their quorums,
    /// the exit window, the cap) that cites it. Citizens alone, on evidence
    /// nobody checks, never take a validator out.
    function closeChallenge(uint256 id) external {
        Challenge storage x = challenges[id];
        require(block.timestamp > x.ends && !x.closed, "open, or closed");
        x.closed = true;
        uint256 cast = uint256(x.yes) + x.no;
        uint256 q = (uint256(x.electorate) * rules.quorumBps + 9999) / 10000;
        uint256 floor = (uint256(x.electorate) + 1) / 2;
        if (floor > 5) floor = 5;
        if (q < floor) q = floor;
        x.passed = cast > 0 && cast >= q && uint256(x.yes) * 3 >= cast * 2;
        emit ChallengeClosed(id, x.passed, x.yes, x.no);
    }

    function challengeCount() external view returns (uint256) {
        return challenges.length;
    }

    function challengeAt(uint256 id) external view returns (Challenge memory) {
        return challenges[id];
    }

    // ---- censorship: evidence on chain (by any citizen whose own txs get through)

    /// A transaction a watcher saw pending for blocksPending blocks while
    /// these proposers made blocks without it. Evidence for a removal vote
    /// (or, if that vote is censored too, for the social fork).
    function reportCensorship(bytes32 by, bytes32 txHash, uint64 firstSeenBlock, uint64 blocksPending, address[] calldata proposers,
        string calldata uri) external {
        _citizenOwner(by);
        emit CensorshipReported(txHash, by, firstSeenBlock, blocksPending, proposers, uri);
    }

    // ---- the cap

    /// Changes (adds + removes) applied within the window, and the set as it was before them.
    function churn() public view returns (uint256 inWindow, uint256 setBefore) {
        uint256 since = block.timestamp > rules.window ? block.timestamp - rules.window : 0;
        int256 n = int256(list.length);
        for (uint256 i = changes.length; i > 0; i--) {
            uint64 c = changes[i - 1];
            if (uint256(c & ~ADDED) < since) break; // the window is closed: [now - window, now]
            inWindow++;
            n += (c & ADDED) != 0 ? int256(-1) : int256(1);
        }
        setBefore = uint256(n);
    }

    /// Changes still allowed in this window.
    function changesLeft() public view returns (uint256) {
        (uint256 k, uint256 n0) = churn();
        uint256 cap = n0 / 3;
        if (cap == 0) cap = 1;
        return k >= cap ? 0 : cap - k;
    }

    function _capped() internal view {
        require(changesLeft() > 0, "rolling cap: at most a third of the set changes per window");
    }

    /// After the change: the log, and the alert past a quarter.
    function _record(bool added) internal {
        changes.push(uint64(block.timestamp) | (added ? ADDED : 0));
        (uint256 k, uint256 n0) = churn();
        if (k * 4 > n0) emit ChurnAlert(k, n0, rules.window);
    }

    function changeCount() external view returns (uint256) {
        return changes.length;
    }

    // ---- views

    function upgradeCount() external view returns (uint256) {
        return upgrades.length;
    }

    function upgradeAt(uint256 i) external view returns (Upgrade memory) {
        return upgrades[i];
    }

    // ---- internals

    /// v, or, for a key its citizen rotated away from (same binding), that citizen's current key.
    function _current(address v) internal view returns (address) {
        if (position[v] != 0) return v;
        bytes32 c = formerCitizen[v];
        if (c != 0 && formerBinding[v] == binding[c] && validatorOf[c] != address(0)) return validatorOf[c];
        return v;
    }

    function _schedule(address v, uint8 why) internal {
        require(position[v] != 0, "not a validator");
        require(removalAt[v] == 0, "already scheduled");
        uint64 at = uint64(block.timestamp) + rules.removalDelay;
        removalAt[v] = at;
        removalWhy[v] = why;
        emit RemovalScheduled(v, citizenOf[v], why, at);
    }

    function _owner(bytes32 who) internal view {
        require(address(registry) != address(0), "no governance yet");
        (bool ok, bytes32 o, bool agent) = registry.canAct(who, msg.sender);
        require(ok && !agent && o == who, "the owner's own key only");
    }

    function _citizenOwner(bytes32 who) internal view {
        _owner(who);
        require(citizens.isCitizen(who), "citizens only");
    }

    function _setRules(Rules memory r) internal {
        // minSet >= 4: fewer validators survive no failure (QBFT needs 3f+1).
        require(r.window > 0 && r.minSet >= 4 && r.voteWindow > 0 && r.quorumBps > 0 && r.quorumBps <= 10000, "rules");
        rules = r;
        emit RulesSet(r);
    }

    function _add(address v, bytes32 citizen) internal {
        require(v != address(0) && position[v] == 0 && citizenOf[v] == 0, "zero or already a validator");
        list.push(v);
        position[v] = list.length;
        if (citizen != 0) {
            require(validatorOf[citizen] == address(0), "one validator per citizen");
            citizenOf[v] = citizen;
            validatorOf[citizen] = v;
        }
        emit ValidatorAdded(v, citizen);
    }

    /// Never empties the set: a chain with no validators makes no blocks.
    function _remove(address v) internal {
        uint256 p = position[v];
        require(list.length > 1, "the last validator");
        address last = list[list.length - 1];
        list[p - 1] = last;
        position[last] = p;
        list.pop();
        delete position[v];
        delete removalAt[v];
        delete removalWhy[v];
        bytes32 c = citizenOf[v];
        if (c != 0) {
            delete citizenOf[v];
            delete validatorOf[c];
            binding[c]++; // its old keys no longer lead anywhere
        }
    }
}
