// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import "./CitizenGroup.sol";
import {IEAS, Attestation} from "@ethereum-attestation-service/eas-contracts/contracts/IEAS.sol";

/// Settled deals with distinct counterparties, as some primitive counts them.
/// None at launch: the requirement is off until a vote of both houses names one.
interface IDealCount {
    function counterparties(bytes32 owner) external view returns (uint256);
}

/// A proof-of-personhood source: how strongly it holds that `who` (an owner
/// name) is one person not already counted, in basis points (0: not at all).
/// Vouching by citizens is the built-in source; both houses add others (a
/// zero-knowledge passport proof, an in-person ceremony, another network's
/// registry) and weigh them (Citizens.setSources).
interface IPersonhood {
    function personhood(bytes32 who) external view returns (uint16 bps);
}

/// The citizens' house: one vote per citizen, a citizen being an owner name
/// (never an agent). Citizenship is earned, not bought:
///   - proof of personhood worth 10000 points: K existing citizens vouching
///     for the name is worth vouchWeight (10000 at launch, so vouching alone
///     suffices), and each source governance names adds weight × its bps /
///     10000 (none at launch), and
///   - the name is at least N days old, and
///   - (when a counter is named) it has M settled deals with distinct counterparties.
/// Vouchers put their own standing at risk: if citizens later vote that a
/// name was a sybil, everyone who vouched it in loses citizenship for a
/// penalty period. A sybil ring can't vouch itself in: only citizens vouch.
///
/// Genesis: a small founding set, each bound to the key that must claim it
/// (so a squatter who registers the name first gets nothing). They are
/// provisional: a claimed founder votes in the first term but doesn't vouch
/// for newcomers or judge revocations until other founders (or seasoned
/// citizens) confirmed it, max(K, founders / 2) of them, and it then served
/// probation (minAge from its confirmation) like any new citizen (audit 5
/// GV-2). After the first term only confirmed founders remain. No permanent
/// founders, no admin: the parameters change only through the timelock, that
/// is, by both houses.
contract Citizens {
    Registry public immutable registry;
    address public immutable governance; // the timelock
    uint64 public immutable firstTermEnds;
    /// The citizens' tree for secret votes: one identity leaf per citizen, kept in step here.
    CitizenGroup public immutable group;

    struct Params {
        uint8 vouches; // K: vouches needed
        uint8 maxVouches; // vouches a citizen may give per term
        uint16 quorumBps; // turnout for a revocation, as a share of citizens
        uint16 revokeBps; // share of the votes cast that must say yes to revoke
        uint16 minDeals; // M (counted only when `deals` is set)
        uint64 minAge; // N: seconds since the name was registered
        uint64 voteWindow; // seconds a revocation vote lasts
        uint64 penalty; // seconds a revoked name, or a sybil's voucher, stays out
        uint64 term; // seconds per vouching allowance
        address deals; // an IDealCount, or 0: off
    }
    Params public p;

    struct Citizen {
        uint64 since; // last admission (0: never a citizen)
        uint64 barredUntil; // no citizenship before this
        uint64 termFrom; // its vouching term began
        uint8 vouchesUsed;
        bool active;
        bool genesis;
        bool confirmed; // a genesis citizen re-vouched by others
        bool suspended; // a sybil's voucher: comes back without new vouches after barredUntil
    }
    mapping(bytes32 => Citizen) public citizens;
    mapping(bytes32 => address) public genesisKey; // founding name => the key that may claim it
    mapping(bytes32 => bytes32[]) internal vouchersOf; // citizen => who vouched it in (or confirmed it)
    mapping(bytes32 => bytes32[]) internal pending; // candidate => vouches so far
    mapping(bytes32 => mapping(bytes32 => bool)) public vouched; // candidate => voucher
    uint32 public count; // active citizens

    /// Growth limits (audit 4 GV-1): a new citizen is on probation (no
    /// vouching, no revocation votes) for minAge after admission, however old
    /// its name is; and the network admits at most max(ADMIT_FLOOR,
    /// count × ADMIT_BPS) new citizens per term, so a colluding ring can't
    /// grow geometrically in a block. Revocations need a turnout of at least
    /// min(K + 2, half the electorate), and a sybil's own vouchers don't vote on it.
    uint256 public immutable ADMIT_BPS; // launch 2000; set at deploy (PROOFREAD-6 #30)
    uint256 public immutable ADMIT_FLOOR; // launch 3
    uint64 public admitFrom; // the current admission window began
    /// Personhood: K vouches count vouchWeight points; each source its weight × bps / 10000; 10000 points admit.
    uint16 public vouchWeight = 10000;
    address[] internal sources;
    uint16[] internal weights;
    uint16 public constant PERSONHOOD = 10000;
    uint256 public constant MAX_SOURCES = 8;
    uint32 public admitted; // admissions in it

    /// Fairness between vouching groups (audit 5 GV-1): an admission's vouch
    /// tree is its vouchers plus the citizens who vouched each of them in
    /// (depth 2; founders are roots, their confirmers don't count). A citizen
    /// may be in the trees of at most half the window's admissions (rounded
    /// up), so a ring that vouched its own members in can't take every slot,
    /// while candidates vouched by others still get in; a candidate whose
    /// group is full waits for the next window ('admit'). Depth 2, not the
    /// whole ancestry: every citizen descends from the founders, so once
    /// honest and ring lineages mix, a transitive tree is the whole citizenry
    /// and one group would gate everyone. Escaping depth 2 takes a ring two
    /// more generations, each a probation long.
    mapping(bytes32 => uint64) internal treeWindow; // citizen => the window its count is for
    mapping(bytes32 => uint32) internal treeUsed; // citizen => admissions whose tree it was in, that window
    uint32 public immutable founders; // genesis names
    mapping(bytes32 => uint64) public confirmedAt; // founder => when it was confirmed (its probation runs from then)

    struct Revocation {
        bytes32 who;
        bytes32 by;
        uint64 opened;
        uint64 ends;
        uint32 electorate;
        uint32 yes;
        uint32 no;
        bool sybil;
        bool closed;
        bool passed;
    }
    Revocation[] public revocations;
    mapping(uint256 => mapping(bytes32 => bool)) public revocationVoted;
    mapping(bytes32 => uint256) public openRevocation; // who => revocation + 1

    event GenesisListed(bytes32 indexed who, address key);
    event GenesisClaimed(bytes32 indexed who);
    event Vouched(bytes32 indexed who, bytes32 indexed by, uint256 vouches);
    event Unvouched(bytes32 indexed who, bytes32 indexed by);
    event Admitted(bytes32 indexed who, bytes32[] vouchers);
    event Confirmed(bytes32 indexed who, bytes32[] vouchers);
    event Left(bytes32 indexed who);
    event Lapsed(bytes32 indexed who);
    event Suspended(bytes32 indexed who, uint64 until);
    event Reinstated(bytes32 indexed who);
    event RevocationOpened(uint256 indexed id, bytes32 indexed who, bytes32 indexed by, bool sybil, uint64 ends, string reason);
    event RevocationVoted(uint256 indexed id, bytes32 indexed by, bool yes);
    event RevocationClosed(uint256 indexed id, bytes32 indexed who, bool passed, uint32 yes, uint32 no);
    event ParamsSet(Params p);
    event SourcesSet(address[] sources, uint16[] weights, uint16 vouchWeight);

    constructor(Registry r, address gov, bytes32[] memory genesis, address[] memory keys, uint64 firstTerm, Params memory p_,
        IVoteVerifier voteVerifier, address hasher, uint16 admitBps, uint32 admitFloor) {
        require(admitBps <= 10000 && admitFloor >= 1, "admission");
        ADMIT_BPS = admitBps;
        ADMIT_FLOOR = admitFloor;
        registry = r;
        group = new CitizenGroup(r, voteVerifier, hasher);
        governance = gov;
        _setParams(p_);
        require(genesis.length == keys.length && (genesis.length == 0 || genesis.length > p_.vouches),
            "genesis: one key per name, more names than K");
        for (uint256 i = 0; i < genesis.length; i++) {
            require(keys[i] != address(0) && genesisKey[genesis[i]] == address(0), "genesis key");
            genesisKey[genesis[i]] = keys[i];
            emit GenesisListed(genesis[i], keys[i]);
        }
        firstTermEnds = uint64(block.timestamp) + firstTerm;
        founders = uint32(genesis.length);
    }

    // ---- who is a citizen

    function isCitizen(bytes32 id) public view returns (bool) {
        Citizen storage c = citizens[id];
        return c.active && registry.ownerOf(id) == id && (!c.genesis || c.confirmed || block.timestamp < firstTermEnds);
    }

    /// May vote on something opened at `at`: a citizen now, and already before it opened.
    function canVote(bytes32 id, uint64 at) public view returns (bool) {
        return isCitizen(id) && citizens[id].since < at;
    }

    /// Past probation: a citizen admitted at least minAge ago, or a founder
    /// confirmed at least minAge ago.
    function seasoned(bytes32 id) public view returns (bool) {
        Citizen storage c = citizens[id];
        uint256 from = c.genesis ? (c.confirmed ? confirmedAt[id] : type(uint64).max) : c.since;
        return isCitizen(id) && block.timestamp >= from + p.minAge;
    }

    /// Confirmations a founder needs: max(K, founders / 2).
    function founderVouches() public view returns (uint256) {
        return founders / 2 > p.vouches ? founders / 2 : p.vouches;
    }

    function admitCap() public view returns (uint256 cap) {
        cap = uint256(count) * ADMIT_BPS / 10000;
        if (cap < ADMIT_FLOOR) cap = ADMIT_FLOOR;
    }

    function _window() internal view returns (uint64) {
        return block.timestamp >= uint256(admitFrom) + p.term ? uint64(block.timestamp) : admitFrom;
    }

    /// Admissions left in the current window (founders' re-confirmations don't count).
    function admissionsLeft() public view returns (uint256) {
        uint256 cap = admitCap();
        if (block.timestamp >= uint256(admitFrom) + p.term) return cap;
        return admitted >= cap ? 0 : cap - admitted;
    }

    /// who's vouch tree now: its vouchers in good standing, and who vouched each of them in.
    function _tree(bytes32 who) internal view returns (bytes32[] memory t, uint256 n) {
        bytes32[] storage ps = pending[who];
        uint256 m = ps.length;
        for (uint256 i = 0; i < ps.length; i++) if (!citizens[ps[i]].genesis) m += vouchersOf[ps[i]].length;
        t = new bytes32[](m);
        for (uint256 i = 0; i < ps.length; i++) {
            if (!isCitizen(ps[i])) continue;
            n = _put(t, n, ps[i]);
            if (citizens[ps[i]].genesis) continue;
            bytes32[] storage gs = vouchersOf[ps[i]];
            for (uint256 j = 0; j < gs.length; j++) n = _put(t, n, gs[j]);
        }
    }

    function _put(bytes32[] memory t, uint256 n, bytes32 x) internal pure returns (uint256) {
        for (uint256 i = 0; i < n; i++) if (t[i] == x) return n;
        t[n] = x;
        return n + 1;
    }

    /// May who's vouch tree take another admission this window?
    function groupRoom(bytes32 who) public view returns (bool) {
        (bytes32[] memory t, uint256 n) = _tree(who);
        uint64 w = _window();
        uint256 cap = (admitCap() + 1) / 2;
        for (uint256 i = 0; i < n; i++) if (treeWindow[t[i]] == w && treeUsed[t[i]] >= cap) return false;
        return true;
    }

    function vouchers(bytes32 id) external view returns (bytes32[] memory) {
        return vouchersOf[id];
    }

    function pendingVouches(bytes32 id) external view returns (bytes32[] memory) {
        return pending[id];
    }

    /// The caller is the name's own key: owners only, never an agent.
    function _owner(bytes32 who) internal view {
        (bool ok, bytes32 o, bool agent) = registry.canAct(who, msg.sender);
        require(ok && !agent && o == who, "the owner's own key only");
    }

    // ---- genesis

    function claimGenesis(bytes32 who) external {
        _owner(who);
        Citizen storage c = citizens[who];
        require(genesisKey[who] == msg.sender && block.timestamp < firstTermEnds && c.since == 0, "not a founding name of this key");
        c.genesis = true;
        _activate(who, c);
        emit GenesisClaimed(who);
    }

    /// Anyone retires a founding citizen nobody re-confirmed in the first term.
    function lapse(bytes32 who) external {
        Citizen storage c = citizens[who];
        require(c.active && c.genesis && !c.confirmed && block.timestamp >= firstTermEnds, "not lapsed");
        c.active = false;
        c.genesis = false; // from now on an ordinary name: it may be vouched in like any other
        count--;
        group.sync(who);
        emit Lapsed(who);
    }

    // ---- vouching and admission

    function vouch(bytes32 by, bytes32 who) external {
        _owner(by);
        require(isCitizen(by), "only citizens vouch");
        Citizen storage c = citizens[who];
        bool confirming = _confirming(c); // a claimed founder: other founders confirm it, without probation
        require(seasoned(by) || (confirming && citizens[by].genesis), "on probation: new citizens and unconfirmed founders vouch after minAge");
        require(who != by && registry.ownerOf(who) == who, "an owner name, not yourself or an agent");
        require(!c.active || confirming, "already a citizen");
        require(!vouched[who][by], "vouched already");
        if (!confirming) { // confirming a founder doesn't use up vouches
            Citizen storage v = citizens[by];
            if (block.timestamp >= uint256(v.termFrom) + p.term) {
                v.termFrom = uint64(block.timestamp);
                v.vouchesUsed = 0;
            }
            require(v.vouchesUsed < p.maxVouches, "no vouches left this term");
            v.vouchesUsed++;
        }
        vouched[who][by] = true;
        pending[who].push(by);
        emit Vouched(who, by, pending[who].length);
        if (eligible(who) && (confirming || (admissionsLeft() > 0 && groupRoom(who)))) _admit(who);
    }

    function _confirming(Citizen storage c) internal view returns (bool) {
        return c.active && c.genesis && !c.confirmed;
    }

    function unvouch(bytes32 by, bytes32 who) external {
        _owner(by);
        require(vouched[who][by], "no vouch");
        vouched[who][by] = false;
        bytes32[] storage ps = pending[who];
        for (uint256 i = 0; i < ps.length; i++) {
            if (ps[i] == by) {
                ps[i] = ps[ps.length - 1];
                ps.pop();
                break;
            }
        }
        if (!_confirming(citizens[who]) && citizens[by].vouchesUsed > 0) citizens[by].vouchesUsed--;
        emit Unvouched(who, by);
    }

    /// Vouches that count now: from citizens in good standing.
    function validVouches(bytes32 who) public view returns (uint256 n) {
        bytes32[] storage ps = pending[who];
        for (uint256 i = 0; i < ps.length; i++) {
            if (isCitizen(ps[i])) n++;
        }
    }

    /// Personhood points: vouchWeight with K valid vouches, plus each source's
    /// weight × its bps / 10000 (a source that fails counts nothing).
    function personhood(bytes32 who) public view returns (uint256 points) {
        if (validVouches(who) >= p.vouches) points = vouchWeight;
        for (uint256 i = 0; i < sources.length; i++) {
            try IPersonhood(sources[i]).personhood(who) returns (uint16 bps) {
                if (bps > 10000) bps = 10000;
                points += uint256(weights[i]) * bps / 10000;
            } catch {}
        }
    }

    function personhoodSources() external view returns (address[] memory, uint16[] memory) {
        return (sources, weights);
    }

    /// All conditions met: personhood, age, deals, not barred.
    function eligible(bytes32 who) public view returns (bool) {
        Citizen storage c = citizens[who];
        if (registry.ownerOf(who) != who) return false;
        if (c.active) { // a founder's confirmation: vouches only
            uint256 n = validVouches(who);
            return _confirming(c) && n >= p.vouches && n >= founderVouches();
        }
        if (personhood(who) < PERSONHOOD) return false;
        if (block.timestamp < c.barredUntil) return false;
        if (block.timestamp < uint256(registry.registeredAt(who)) + p.minAge) return false;
        if (p.deals != address(0) && IDealCount(p.deals).counterparties(who) < p.minDeals) return false;
        return true;
    }

    /// Anyone admits a name once it is eligible (when its age comes after its vouches).
    function admit(bytes32 who) external {
        require(eligible(who), "not eligible yet");
        require(citizens[who].active || admissionsLeft() > 0, "this term's admissions are full: try in the next");
        require(citizens[who].active || groupRoom(who), "this vouching group's admissions are full this term: try in the next");
        _admit(who);
    }

    function _admit(bytes32 who) internal {
        if (!citizens[who].active) _countTree(who);
        bytes32[] storage ps = pending[who];
        bytes32[] storage vs = vouchersOf[who];
        delete vouchersOf[who];
        for (uint256 i = 0; i < ps.length; i++) {
            vouched[who][ps[i]] = false;
            if (isCitizen(ps[i])) vs.push(ps[i]);
        }
        delete pending[who];
        Citizen storage c = citizens[who];
        if (c.active) {
            c.confirmed = true;
            confirmedAt[who] = uint64(block.timestamp);
            emit Confirmed(who, vs);
            return;
        }
        c.suspended = false;
        if (block.timestamp >= uint256(admitFrom) + p.term) {
            admitFrom = uint64(block.timestamp);
            admitted = 0;
        }
        admitted++;
        _activate(who, c);
        emit Admitted(who, vs);
    }

    /// Counts an admission against everyone in its vouch tree, this window.
    function _countTree(bytes32 who) internal {
        (bytes32[] memory t, uint256 n) = _tree(who);
        uint64 w = _window();
        for (uint256 i = 0; i < n; i++) {
            if (treeWindow[t[i]] != w) {
                treeWindow[t[i]] = w;
                treeUsed[t[i]] = 0;
            }
            treeUsed[t[i]]++;
        }
    }

    function _activate(bytes32 who, Citizen storage c) internal {
        c.active = true;
        c.since = uint64(block.timestamp);
        count++;
        group.sync(who);
    }

    /// A sybil's voucher comes back by itself after the penalty.
    function reinstate(bytes32 who) external {
        Citizen storage c = citizens[who];
        require(c.suspended && !c.active && block.timestamp >= c.barredUntil && registry.ownerOf(who) == who, "not suspended or too early");
        c.suspended = false;
        _activate(who, c);
        emit Reinstated(who);
    }

    function leave(bytes32 who) external {
        _owner(who);
        Citizen storage c = citizens[who];
        require(c.active, "not a citizen");
        c.active = false;
        count--;
        group.sync(who);
        emit Left(who);
    }

    // ---- revocation: a citizens' vote

    function openRevocationVote(bytes32 by, bytes32 who, bool sybil, string calldata reason) external returns (uint256 id) {
        _owner(by);
        require(seasoned(by), "only citizens past probation");
        require(by != who && citizens[who].since != 0 && openRevocation[who] == 0, "not a citizen, or a vote is open");
        id = revocations.length;
        uint64 ends = uint64(block.timestamp) + p.voteWindow;
        uint32 electorate = count; // a sybil case: the name and its vouchers don't vote, so they don't count
        if (sybil) {
            if (isCitizen(who) && electorate > 0) electorate--;
            bytes32[] storage vs = vouchersOf[who];
            for (uint256 i = 0; i < vs.length && electorate > 0; i++) if (isCitizen(vs[i])) electorate--;
        }
        revocations.push(Revocation(who, by, uint64(block.timestamp), ends, electorate, 0, 0, sybil, false, false));
        openRevocation[who] = id + 1;
        emit RevocationOpened(id, who, by, sybil, ends, reason);
    }

    function voteRevocation(uint256 id, bytes32 by, bool yes) external {
        _owner(by);
        Revocation storage r = revocations[id];
        require(block.timestamp <= r.ends && !r.closed, "vote closed");
        require(by != r.who && canVote(by, r.opened) && seasoned(by), "citizens from before the vote only, past probation");
        if (r.sybil) {
            bytes32[] storage vs = vouchersOf[r.who];
            for (uint256 i = 0; i < vs.length; i++) require(vs[i] != by, "you vouched for this name: not your vote");
        }
        require(!revocationVoted[id][by], "voted");
        revocationVoted[id][by] = true;
        if (yes) r.yes++;
        else r.no++;
        emit RevocationVoted(id, by, yes);
    }

    function closeRevocation(uint256 id) external {
        Revocation storage r = revocations[id];
        require(block.timestamp > r.ends && !r.closed, "vote open");
        r.closed = true;
        openRevocation[r.who] = 0;
        uint256 cast = uint256(r.yes) + r.no;
        uint256 q = (uint256(r.electorate) * p.quorumBps + 9999) / 10000;
        uint256 floor = (uint256(r.electorate) + 1) / 2; // a tiny set never decides alone: K + 2, or half
        if (floor > uint256(p.vouches) + 2) floor = uint256(p.vouches) + 2;
        if (q < floor) q = floor;
        r.passed = cast >= q && cast > 0 && uint256(r.yes) * 10000 >= cast * p.revokeBps && r.yes > r.no;
        emit RevocationClosed(id, r.who, r.passed, r.yes, r.no);
        if (!r.passed) return;
        uint64 until = uint64(block.timestamp) + p.penalty;
        Citizen storage c = citizens[r.who];
        if (c.active) {
            c.active = false;
            count--;
        }
        c.suspended = false;
        if (c.barredUntil < until) c.barredUntil = until;
        group.sync(r.who);
        emit Suspended(r.who, until);
        if (!r.sybil) return;
        bytes32[] storage vs = vouchersOf[r.who];
        for (uint256 i = 0; i < vs.length; i++) {
            Citizen storage v = citizens[vs[i]];
            if (v.active) {
                v.active = false;
                v.suspended = true;
                count--;
            }
            if (v.barredUntil < until) v.barredUntil = until;
            group.sync(vs[i]);
            emit Suspended(vs[i], until);
        }
        delete vouchersOf[r.who];
    }

    function revocationCount() external view returns (uint256) {
        return revocations.length;
    }

    // ---- parameters: by both houses, through the timelock

    function setParams(Params calldata p_) external {
        require(msg.sender == governance, "governance only");
        _setParams(p_);
    }

    /// Which personhood sources count and how much (both houses). At most
    /// MAX_SOURCES; weights in points (10000 alone admits).
    function setSources(address[] calldata s, uint16[] calldata w, uint16 vouchWeight_) external {
        require(msg.sender == governance, "governance only");
        require(s.length == w.length && s.length <= MAX_SOURCES && vouchWeight_ <= PERSONHOOD, "sources");
        for (uint256 i = 0; i < s.length; i++) require(s[i].code.length > 0 && w[i] <= PERSONHOOD, "sources");
        sources = s;
        weights = w;
        vouchWeight = vouchWeight_;
        emit SourcesSet(s, w, vouchWeight_);
    }

    function _setParams(Params memory p_) internal {
        require(p_.vouches > 0 && p_.maxVouches > 0 && p_.quorumBps <= 10000 && p_.revokeBps > 5000 && p_.revokeBps <= 10000
            && p_.voteWindow > 0 && p_.term > 0, "params");
        p = p_;
        emit ParamsSet(p_);
    }
}

/// A personhood source from claims: one issuer (a Tern name: a passport-proof
/// verifier, an in-person ceremony's organiser) attests claims of kind
/// "personhood" about owner names in the Tern claim schema, score = bps. A name
/// (or anyone) registers its claim here; it counts while it is unrevoked,
/// unexpired and its attester still acts for the issuer. Anyone deploys one
/// per issuer; both houses decide whether it counts (Citizens.setSources).
contract ClaimPersonhood is IPersonhood {
    IEAS public immutable eas;
    Registry public immutable registry;
    bytes32 public immutable issuer;
    bytes32 public immutable schema; // the claim schema
    mapping(bytes32 => bytes32) public claimOf; // who => attestation uid

    event Registered(bytes32 indexed who, bytes32 uid, uint16 bps);

    constructor(IEAS e, Registry r, bytes32 issuer_, bytes32 schema_) {
        eas = e;
        registry = r;
        issuer = issuer_;
        schema = schema_;
    }

    function register(bytes32 who, bytes32 uid) external {
        uint16 bps = _score(who, uid);
        require(bps > 0, "not a personhood claim about that name from the issuer");
        claimOf[who] = uid;
        emit Registered(who, uid, bps);
    }

    function personhood(bytes32 who) external view returns (uint16) {
        bytes32 uid = claimOf[who];
        return uid == 0 ? 0 : _score(who, uid);
    }

    function _score(bytes32 who, bytes32 uid) internal view returns (uint16) {
        Attestation memory a = eas.getAttestation(uid);
        if (a.schema != schema || a.revocationTime != 0 || (a.expirationTime != 0 && a.expirationTime <= block.timestamp)) return 0;
        (bool ok,,) = registry.canAct(issuer, a.attester);
        if (!ok) return 0;
        (bytes32 subject, string memory kind, , int64 score) = abi.decode(a.data, (bytes32, string, bytes, int64));
        if (subject != who || keccak256(bytes(kind)) != keccak256("personhood") || score <= 0 || score > 10000) return 0;
        return uint16(uint64(score));
    }
}
