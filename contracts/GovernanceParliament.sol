// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import {Strings} from "@openzeppelin/contracts/utils/Strings.sol";

interface ICoinHouse {
    function propose(address[] memory targets, uint256[] memory values, bytes[] memory calldatas, string memory description)
        external returns (uint256);
    function state(uint256 proposalId) external view returns (uint8);
    function proposalDeadline(uint256 proposalId) external view returns (uint256);
    function proposalVotes(uint256 proposalId) external view returns (uint256 against, uint256 forVotes, uint256 abstain);
}

interface ITimelock {
    function schedule(address target, uint256 value, bytes calldata data, bytes32 predecessor, bytes32 salt, uint256 delay) external;
    function execute(address target, uint256 value, bytes calldata payload, bytes32 predecessor, bytes32 salt) external payable;
    function hashOperation(address target, uint256 value, bytes calldata data, bytes32 predecessor, bytes32 salt)
        external pure returns (bytes32);
    function getMinDelay() external view returns (uint256);
    function getTimestamp(bytes32 id) external view returns (uint256);
    function isOperationReady(bytes32 id) external view returns (bool);
    function isOperationDone(bytes32 id) external view returns (bool);
}

interface ICitizenHouse {
    function isCitizen(bytes32 id) external view returns (bool);
    function canVote(bytes32 id, uint64 at) external view returns (bool);
    function count() external view returns (uint32);
    function group() external view returns (ICitizenTree);
}

/// The citizens' tree (CitizenGroup): identity leaves of current citizens.
interface ICitizenTree {
    function root() external view returns (uint256);
    function registrations() external view returns (uint256);
    function seqOf(bytes32 who) external view returns (uint256);
    function verifyVote(uint256 rootOpen, uint256 rootNow, uint256 nullifier, uint256 scope, uint256 message, bytes calldata proof) external view;
}

interface IVoteWeight {
    function getVotes(address account) external view returns (uint256);
}

interface IDirectoryPublish {
    function publish(string calldata key, address value, bytes32 spec, string calldata specURI, bytes32 audit,
        string calldata auditURI, string calldata notes) external;
}

interface ITreasuryGrant {
    function grant(uint256 proposal, bytes32 payee, uint256 amount, uint64 refundAfter, address condition, bytes calldata demand,
        string calldata terms) external returns (uint256);
}

/// Two houses decide every change to the network: the coin house (wTERN
/// votes, counted by TernGovernor) and the citizens' house (one vote per
/// citizen). A proposal passes only if BOTH reach their quorum and a
/// majority in the same voting window; either house alone can stop it. Then
/// it waits in the timelock for the exit window, and anyone executes it.
///
/// What a proposal does is built here from its kind, so what voters read is
/// what runs:
///   upgrade  Directory.publish(key, address, spec, audit, notes): a new
///            current version; old versions stay and keep working
///   param    one call to a governed contract (fee oracle targets, citizen
///            rules, these houses' own settings, the timelock's delay)
///   grant    Treasury.grant: treasury money into a Bank lock decided by
///            reviewers (or any condition), refundable to the treasury
/// No one holds a key to any of it, the deployer included.
///
/// Governance-critical proposals (docs/abuse/VALIDATORS.md, F6): an upgrade of
/// a governance key (Validators, Parliament, Citizens, CitizenGroup,
/// Directory, TernTimelock, TernGovernor, WrappedTern, VoteVerifier,
/// FeeOracle: the price of every message, audit 6 FO-3), or a param call to
/// the timelock, these houses, the citizens' house, the coin house, or any
/// version of those keys (Validators: other than its capped set changes,
/// addValidator, removeValidator, cancelRemoval). They need SUPER_NUM /
/// SUPER_DEN (2/3) of the votes cast in BOTH houses (on top of each house's
/// quorum) and wait CRITICAL_DELAY_X times the exit window. Clients show them
/// loudly. These three numbers and MIN_TURNOUT are set at deploy (Rules) and
/// fixed for this Parliament (PROOFREAD-6 #28).
contract Parliament {
    enum Kind { Upgrade, Param, Grant }
    enum State { Voting, Defeated, Passed, Queued, Ready, Executed, Expired }

    struct Proposal {
        Kind kind;
        bytes32 by; // the citizen who proposed (0: a coin holder)
        address proposer;
        address target;
        bytes data; // the one call the timelock makes
        uint256 coinId; // its twin in the coin house
        uint64 opened;
        uint64 ends;
        uint32 electorate; // citizens when it opened
        uint32 yes;
        uint32 no;
        bool queued;
        string title;
    }

    Registry public immutable registry;
    ICitizenHouse public immutable citizens;
    ICoinHouse public immutable coinHouse;
    ITimelock public immutable timelock;
    IVoteWeight public immutable token;
    address public immutable directory;
    address public immutable treasury;

    uint256 public proposeThreshold; // wTERN votes a coin holder needs to propose (citizens always may)
    uint16 public citizenQuorumBps; // turnout in the citizens' house, as a share of citizens
    uint64 public queueWindow; // a passed proposal must be queued within this

    Proposal[] internal props;
    mapping(uint256 => mapping(bytes32 => bool)) public citizenVoted;
    /// Upgrades: the Directory key each one publishes, and per key the newest
    /// upgrade executed (+1). An older upgrade can't run after a newer one of
    /// the same key: no rollback by a stale op (audit 4 GV-4).
    mapping(uint256 => bytes32) public upgradeKeyOf;
    mapping(bytes32 => uint256) public lastUpgrade;
    /// Secret votes (Semaphore): the citizens' tree when each proposal opened,
    /// how many identities were registered then, and the nullifiers used.
    mapping(uint256 => uint256) public citizenRootAt;
    mapping(uint256 => uint256) public registrationsAt;
    mapping(uint256 => mapping(uint256 => bool)) public nullifierUsed;
    /// Governance-critical: a supermajority and a longer exit window.
    mapping(uint256 => bool) public critical;
    uint256 public immutable CRITICAL_DELAY_X; // launch: 3
    uint256 public immutable SUPER_NUM; // launch: 2 (of 3)
    uint256 public immutable SUPER_DEN; // launch: 3

    /// Deploy-time rules (fixed for this Parliament).
    struct Rules {
        uint32 criticalDelayX;
        uint32 minTurnout;
        uint8 superNum;
        uint8 superDen;
    }

    event Proposed(uint256 indexed id, Kind kind, bytes32 indexed by, address proposer, address target, uint256 coinId,
        uint64 ends, string title, bytes data);
    event UpgradeProposed(uint256 indexed id, string key, address value, bytes32 spec, string specURI, bytes32 audit,
        string auditURI, string notes);
    event GrantProposed(uint256 indexed id, bytes32 indexed payee, uint256 amount, address condition, uint64 refundAfter, string terms);
    event CitizenVoted(uint256 indexed id, bytes32 indexed by, bool yes);
    event SecretVoted(uint256 indexed id, uint256 nullifier, bool yes);
    event Critical(uint256 indexed id, string why);
    event Queued(uint256 indexed id, uint256 eta);
    event Executed(uint256 indexed id);
    event SettingsSet(uint256 proposeThreshold, uint16 citizenQuorumBps, uint64 queueWindow);

    constructor(Registry r, ICitizenHouse c, ICoinHouse g, ITimelock t, IVoteWeight w, address dir, address tre,
        uint256 threshold, uint16 quorumBps, uint64 queueWindow_, Rules memory rules) {
        require(rules.criticalDelayX >= 1 && rules.criticalDelayX <= 10 && rules.minTurnout >= 1 && rules.superDen > 0
            && rules.superNum <= rules.superDen && uint256(rules.superNum) * 2 > rules.superDen, "rules");
        CRITICAL_DELAY_X = rules.criticalDelayX;
        MIN_TURNOUT = rules.minTurnout;
        SUPER_NUM = rules.superNum;
        SUPER_DEN = rules.superDen;
        registry = r;
        citizens = c;
        coinHouse = g;
        timelock = t;
        token = w;
        directory = dir;
        treasury = tre;
        _settings(threshold, quorumBps, queueWindow_);
    }

    // ---- proposing

    function proposeUpgrade(bytes32 by, string calldata key, address value, bytes32 spec, string calldata specURI, bytes32 audit,
        string calldata auditURI, string calldata notes, string calldata title) external returns (uint256 id) {
        require(value.code.length > 0, "not a contract");
        require(spec != 0 && audit != 0 && bytes(specURI).length > 0 && bytes(auditURI).length > 0, "a spec and an audit report");
        id = _open(Kind.Upgrade, by, directory,
            abi.encodeCall(IDirectoryPublish.publish, (key, value, spec, specURI, audit, auditURI, notes)), title);
        upgradeKeyOf[id] = keccak256(bytes(key));
        if (_criticalKey(key)) _critical(id, string.concat("an upgrade of ", key));
        emit UpgradeProposed(id, key, value, spec, specURI, audit, auditURI, notes);
    }

    /// One call to a governed contract. Upgrades and grants have their own
    /// kinds (with their spec, audit and escrow), so not the Directory or the treasury.
    function proposeParam(bytes32 by, address target, bytes calldata data, string calldata title) external returns (uint256) {
        // Not the timelock (roles, delay: a proposer role would bypass both houses), nor the
        // Registry (the treasury's name) (audit 4 GV-3); the delay changes through updateDelay only.
        require(target != directory && target != treasury && target != address(registry) && target.code.length > 0 && data.length >= 4,
            "target");
        require(target != address(timelock) || bytes4(data[:4]) == bytes4(keccak256("updateDelay(uint256)")), "the timelock: its delay only");
        uint256 id = _open(Kind.Param, by, target, data, title);
        string memory why = _criticalTarget(target, bytes4(data[:4]));
        if (bytes(why).length > 0) _critical(id, why);
        return id;
    }

    function _critical(uint256 id, string memory why) internal {
        critical[id] = true;
        emit Critical(id, why);
    }

    function _criticalKeys() internal pure returns (string[10] memory) {
        return [string("Validators"), "Parliament", "Citizens", "CitizenGroup", "Directory", "TernTimelock", "TernGovernor", "WrappedTern",
            "VoteVerifier", "FeeOracle"];
    }

    function _criticalKey(string calldata key) internal pure returns (bool) {
        string[10] memory ks = _criticalKeys();
        for (uint256 i = 0; i < ks.length; i++) {
            if (keccak256(bytes(ks[i])) == keccak256(bytes(key))) return true;
        }
        return false;
    }

    /// Why a param call is governance-critical ("": it isn't).
    function _criticalTarget(address target, bytes4 sel) internal view returns (string memory) {
        if (target == address(timelock)) return "the exit window";
        if (target == address(this)) return "Parliament's settings";
        if (target == address(citizens) || target == address(citizens.group())) return "the citizens' house";
        if (target == address(coinHouse) || target == address(token)) return "the coin house";
        string[10] memory ks = _criticalKeys();
        for (uint256 i = 0; i < ks.length; i++) {
            if (IDirectoryRead(directory).versionOf(ks[i], target) == 0) continue;
            if (i == 0 && (sel == bytes4(keccak256("addValidator(address,bytes32)")) || sel == bytes4(keccak256("removeValidator(address)"))
                    || sel == bytes4(keccak256("cancelRemoval(address)")))) return ""; // the capped path
            return string.concat("a governance contract: ", ks[i]);
        }
        return "";
    }

    function proposeGrant(bytes32 by, bytes32 payee, uint256 amount, uint64 refundAfter, address condition, bytes calldata demand,
        string calldata terms) external returns (uint256 id) {
        require(amount > 0 && condition.code.length > 0, "amount or condition");
        id = props.length;
        _open(Kind.Grant, by, treasury,
            abi.encodeCall(ITreasuryGrant.grant, (id, payee, amount, refundAfter, condition, demand, terms)), terms);
        require(refundAfter > props[id].ends + timelock.getMinDelay(), "refund time before it could open");
        emit GrantProposed(id, payee, amount, condition, refundAfter, terms);
    }

    function _open(Kind kind, bytes32 by, address target, bytes memory data, string calldata title) internal returns (uint256 id) {
        if (by != 0) {
            _owner(by);
            require(citizens.isCitizen(by), "citizens propose");
        } else {
            require(token.getVotes(msg.sender) >= proposeThreshold, "below the proposal threshold");
        }
        id = props.length;
        address[] memory targets = new address[](1);
        targets[0] = address(this);
        uint256[] memory values = new uint256[](1);
        bytes[] memory calls = new bytes[](1);
        calls[0] = abi.encodeCall(this.twin, (id));
        // "#proposer=": only Parliament can open this twin, so nobody front-runs it (audit 4 GV-5).
        uint256 coinId = coinHouse.propose(targets, values, calls, string.concat("Tern proposal ", Strings.toString(id), ": ", title,
            "#proposer=", Strings.toHexString(address(this))));
        uint64 ends = uint64(coinHouse.proposalDeadline(coinId));
        props.push(Proposal(kind, by, msg.sender, target, data, coinId, uint64(block.timestamp), ends, citizens.count(), 0, 0, false, title));
        ICitizenTree g = citizens.group();
        citizenRootAt[id] = g.root();
        registrationsAt[id] = g.registrations();
        emit Proposed(id, kind, by, msg.sender, target, coinId, ends, title, data);
    }

    /// What the coin house's twin proposal points at; it is never run.
    function twin(uint256) external pure {}

    function _owner(bytes32 who) internal view {
        (bool ok, bytes32 o, bool agent) = registry.canAct(who, msg.sender);
        require(ok && !agent && o == who, "the owner's own key only");
    }

    // ---- voting: coin holders vote on coinId in TernGovernor; citizens here

    /// An open vote, by name: only for a citizen who had no voting identity
    /// when the proposal opened (one who had votes by proof, below), so no
    /// one votes both ways.
    function voteCitizen(uint256 id, bytes32 by, bool yes) external {
        _owner(by);
        Proposal storage x = props[id];
        require(block.timestamp <= x.ends, "voting closed");
        require(citizens.canVote(by, x.opened), "citizens from before the proposal only");
        require(!citizenVoted[id][by], "voted");
        uint256 seq = citizens.group().seqOf(by);
        require(seq == 0 || seq > registrationsAt[id], "you have a voting identity: vote by proof (secret, or named with --open)");
        citizenVoted[id][by] = true;
        if (yes) x.yes++;
        else x.no++;
        emit CitizenVoted(id, by, yes);
    }

    /// A secret vote (Semaphore): a proof that some identity was in the
    /// citizens' tree when the proposal opened and is in a recent root
    /// (rootNow: CitizenGroup.knownRoot), with nullifier = H(scope, secret),
    /// one per identity and proposal. Anyone may submit it (a relayer, so the
    /// voter's address stays off chain).
    function voteSecret(uint256 id, bool yes, uint256 nullifier, uint256 rootNow, bytes calldata proof) external {
        _secret(id, yes, nullifier, rootNow, voteMessage(yes), proof);
        emit SecretVoted(id, nullifier, yes);
    }

    /// The same proof, with the voter's name on it (gov vote --open, for a
    /// citizen with an identity): counted once by its nullifier. Its message
    /// is keccak(abi.encode(yes, by)) >> 8, so the proof names `by` and
    /// nobody can file it under another name (audit 5 ZK-3).
    function voteNamed(uint256 id, bytes32 by, bool yes, uint256 nullifier, uint256 rootNow, bytes calldata proof) external {
        _owner(by);
        _secret(id, yes, nullifier, rootNow, uint256(keccak256(abi.encode(yes, by))) >> 8, proof);
        emit CitizenVoted(id, by, yes);
    }

    function _secret(uint256 id, bool yes, uint256 nullifier, uint256 rootNow, uint256 message, bytes calldata proof) internal {
        Proposal storage x = props[id];
        require(block.timestamp <= x.ends, "voting closed");
        require(!nullifierUsed[id][nullifier], "voted");
        citizens.group().verifyVote(citizenRootAt[id], rootNow, nullifier, voteScope(id), message, proof);
        nullifierUsed[id][nullifier] = true;
        if (yes) x.yes++;
        else x.no++;
    }

    /// Semaphore's scope and message, hashed into the field (keccak >> 8).
    function voteScope(uint256 id) public view returns (uint256) {
        return uint256(keccak256(abi.encode(address(this), id))) >> 8;
    }

    function voteMessage(bool yes) public pure returns (uint256) {
        return uint256(keccak256(abi.encode(yes))) >> 8;
    }

    /// The citizens' turnout is never below min(MIN_TURNOUT, half the
    /// electorate): one founder can't pass anything alone (audit 4 GV-1, GV-6).
    uint256 public immutable MIN_TURNOUT; // launch: 5

    function citizensPassed(uint256 id) public view returns (bool) {
        Proposal storage x = props[id];
        uint256 cast = uint256(x.yes) + x.no;
        uint256 q = (uint256(x.electorate) * citizenQuorumBps + 9999) / 10000;
        uint256 floor = (uint256(x.electorate) + 1) / 2;
        if (floor > MIN_TURNOUT) floor = MIN_TURNOUT;
        if (q < floor) q = floor;
        if (critical[id]) return cast > 0 && cast >= q && uint256(x.yes) * SUPER_DEN >= cast * SUPER_NUM; // a supermajority
        return cast > 0 && cast >= q && x.yes > x.no;
    }

    function coinPassed(uint256 id) public view returns (bool) {
        uint8 s = coinHouse.state(props[id].coinId);
        if (s != 4 && s != 7) return false; // Succeeded (or someone ran its no-op twin)
        if (!critical[id]) return true;
        (uint256 against, uint256 forVotes,) = coinHouse.proposalVotes(props[id].coinId);
        return forVotes * SUPER_DEN >= (forVotes + against) * SUPER_NUM;
    }

    /// The exit window this proposal waits once queued.
    function exitWindow(uint256 id) public view returns (uint256) {
        uint256 d = timelock.getMinDelay();
        return critical[id] ? d * CRITICAL_DELAY_X : d;
    }

    function state(uint256 id) public view returns (State) {
        Proposal storage x = props[id];
        if (block.timestamp <= x.ends) return State.Voting;
        if (!x.queued) {
            if (!coinPassed(id) || !citizensPassed(id)) return State.Defeated;
            if (block.timestamp > uint256(x.ends) + queueWindow) return State.Expired;
            return State.Passed;
        }
        bytes32 op = operation(id);
        if (timelock.isOperationDone(op)) return State.Executed;
        // Executed within queueWindow of becoming ready, or never (audit 4 GV-4).
        if (block.timestamp > timelock.getTimestamp(op) + queueWindow) return State.Expired;
        if (x.kind == Kind.Upgrade && lastUpgrade[upgradeKeyOf[id]] > id) return State.Expired; // a newer upgrade of its key ran
        if (timelock.isOperationReady(op)) return State.Ready;
        return State.Queued;
    }

    // ---- the exit window, then anyone executes

    function queue(uint256 id) external {
        require(state(id) == State.Passed, "not passed by both houses");
        Proposal storage x = props[id];
        x.queued = true;
        uint256 delay = exitWindow(id);
        timelock.schedule(x.target, 0, x.data, 0, _salt(id), delay);
        emit Queued(id, block.timestamp + delay);
    }

    function execute(uint256 id) external {
        require(state(id) == State.Ready, "not ready: the exit window is open, or it expired");
        Proposal storage x = props[id];
        if (x.kind == Kind.Upgrade) lastUpgrade[upgradeKeyOf[id]] = id + 1;
        timelock.execute(x.target, 0, x.data, 0, _salt(id));
        emit Executed(id);
    }

    function operation(uint256 id) public view returns (bytes32) {
        Proposal storage x = props[id];
        return timelock.hashOperation(x.target, 0, x.data, 0, _salt(id));
    }

    function eta(uint256 id) external view returns (uint256) {
        return props[id].queued ? timelock.getTimestamp(operation(id)) : 0;
    }

    function _salt(uint256 id) internal view returns (bytes32) {
        return keccak256(abi.encode(address(this), id));
    }

    function proposal(uint256 id) external view returns (Proposal memory) {
        return props[id];
    }

    function proposalCount() external view returns (uint256) {
        return props.length;
    }

    // ---- settings: by both houses

    function setSettings(uint256 threshold, uint16 quorumBps, uint64 queueWindow_) external {
        require(msg.sender == address(timelock), "governance only");
        _settings(threshold, quorumBps, queueWindow_);
    }

    function _settings(uint256 threshold, uint16 quorumBps, uint64 queueWindow_) internal {
        require(quorumBps > 0 && quorumBps <= 10000 && queueWindow_ > 0, "settings");
        proposeThreshold = threshold;
        citizenQuorumBps = quorumBps;
        queueWindow = queueWindow_;
        emit SettingsSet(threshold, quorumBps, queueWindow_);
    }
}

interface ITreasuryBank {
    function deposit(bytes32 owner) external payable;
    function balance(bytes32 owner) external view returns (uint256);
    function withdraw(bytes32 owner, address payable to, uint256 amount) external;
    function lock(bytes32 from, bytes32 payee, uint256 total, uint64 refundAfter, address condition, bytes calldata demand)
        external returns (uint256);
}

interface IDirectoryRead {
    function current(string calldata key) external view returns (address);
    function versionOf(string calldata key, address value) external view returns (uint256);
}

/// The network's treasury: fees and donations. It pays only what both houses
/// passed, and only into a Bank lock: money set aside for the payee, released
/// by the lock's condition (reviewers ruling on delivered work, or another
/// condition), refundable to the treasury after the deadline. It holds a
/// Registry name of its own so the Bank sees it as a payer; its guardian is
/// the timelock. It uses whichever Bank the Directory says is current.
contract Treasury {
    address public immutable governance; // the timelock
    Registry public immutable registry;
    IDirectoryRead public immutable directory;
    bytes32 public immutable id; // its name

    event Received(address indexed from, uint256 amount);
    event Granted(uint256 indexed proposal, bytes32 indexed payee, uint256 amount, address bank, uint256 lock, string terms);
    event Reclaimed(address indexed bank, uint256 amount);

    constructor(Registry r, IDirectoryRead d, address gov, string memory name) {
        governance = gov;
        registry = r;
        directory = d;
        id = r.register(name, gov);
    }

    receive() external payable {
        emit Received(msg.sender, msg.value);
    }

    function grant(uint256 proposal, bytes32 payee, uint256 amount, uint64 refundAfter, address condition, bytes calldata demand,
        string calldata terms) external returns (uint256 lock) {
        require(msg.sender == governance, "governance only");
        ITreasuryBank bank = ITreasuryBank(directory.current("Bank"));
        uint256 have = bank.balance(id);
        if (have < amount) bank.deposit{value: amount - have}(id);
        lock = bank.lock(id, payee, amount, refundAfter, condition, demand);
        emit Granted(proposal, payee, amount, address(bank), lock, terms);
    }

    /// Anyone brings refunded grant money back from any Bank version.
    function reclaim(address bank) external {
        require(directory.versionOf("Bank", bank) != 0, "not a Bank version");
        uint256 b = ITreasuryBank(bank).balance(id);
        require(b > 0, "nothing there");
        ITreasuryBank(bank).withdraw(id, payable(address(this)), b);
        emit Reclaimed(bank, b);
    }
}
