// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import "./Sets.sol";

/// A citizens' house: who is a citizen (vouched-in humans, one per person).
interface ICitizensHouse {
    function isCitizen(bytes32 id) external view returns (bool);
}

interface IBankPay {
    function pay(bytes32 from, bytes32 to, uint256 amount, bytes32 memo) external;
    function balance(bytes32 owner) external view returns (uint256);
    function isPayee(bytes32 id) external view returns (bool);
    function lock(bytes32 from, bytes32 payee, uint256 total, uint64 refundAfter, address condition, bytes calldata demand)
        external returns (uint256 id);
}

/// The storage, events and rules shared by Commons and CommonsVotes. Commons
/// runs CommonsVotes' code on its own storage (delegatecall), so the two are
/// one contract at one address, split only to fit the 24 KB code limit. The
/// layout here is the only one: neither contract declares storage of its own.
abstract contract CommonsBase {
    Registry public immutable registry;
    Sets public immutable sets;
    IBankPay public immutable bank;

    enum Kind { None, Admit, Ban, Hide, Rules, Fee, Spend, Follow, Unban, Commission }
    uint8 constant ADMIN = 1;
    uint8 constant MOD = 2;

    struct Params {
        uint64 term; // seconds a role lasts
        uint64 voteWindow; // seconds an election, a petition or a recall vote lasts
        uint64 vetoWindow; // seconds members may veto an action; also how long a new member waits to vote
        uint16 vetoBps; // vetoes from this share of members reverse an action
        uint16 petitionBps; // signatures from this share of members open a recall
        uint16 quorumBps; // turnout needed for elections and recalls
        uint128 budget; // treasury spend per term, in coin-wei
        bool agentsVote; // may agent keys vote (still one vote per owner)
    }

    struct Community {
        bytes32 id; // its name's id in the Registry
        bytes32 set; // its set's id in Sets
        uint64 created;
        uint32 members; // voting members, counted per owner (a new member counts once it may vote)
        uint128 postFee;
        uint128 spentThisTerm;
        uint64 budgetFrom;
        string rules; // URI or text hash
        Params p;
    }

    struct Action {
        bytes32 community;
        Kind kind;
        bytes32 by;
        bytes32 target; // member, hidden ref, followed list, spend recipient; for Rules the new rules' hash
        uint256 value; // fee, amount; for a Ban of a member, its state before (roles, admission)
        bytes32 prev; // what to restore
        string note;
        uint64 at;
        uint32 vetoes;
        bool reversed;
        bool done; // a spend paid out
        uint32 electorate; // voting members when it was taken: its veto threshold is a share of these
    }

    struct Election {
        bytes32 community;
        uint8 role;
        uint8 seats;
        uint64 ends;
        bytes32[] candidates;
        uint32 turnout;
        bool closed;
    }

    struct Recall {
        bytes32 community;
        bytes32 who;
        uint8 role;
        uint64 opened;
        uint32 signatures;
        uint64 ends; // 0 while collecting signatures
        uint32 yes;
        uint32 no;
        bool closed;
    }

    mapping(bytes32 => Community) public communities;
    mapping(bytes32 => mapping(bytes32 => bool)) public isMember; // community => name
    mapping(bytes32 => mapping(bytes32 => uint32)) internal namesOfOwner; // community => owner => counted member names
    mapping(bytes32 => mapping(bytes32 => mapping(uint8 => uint64))) public roleUntil; // community => name => role => term end
    mapping(bytes32 => mapping(bytes32 => bool)) public banned;
    mapping(bytes32 => bytes32[]) internal following; // community => communities whose bans it follows
    mapping(bytes32 => mapping(bytes32 => bool)) public hidden; // community => ref
    mapping(bytes32 => mapping(bytes32 => uint64)) public suspendedUntil; // community => name: a recall vote on it runs

    Action[] public actions;
    mapping(uint256 => mapping(bytes32 => bool)) public vetoed; // action => owner
    Election[] internal elections;
    mapping(uint256 => mapping(bytes32 => uint32)) public tally; // election => candidate => votes
    mapping(uint256 => mapping(bytes32 => bool)) public votedIn; // election => owner
    Recall[] public recalls;
    mapping(uint256 => mapping(bytes32 => bool)) public signed; // recall => owner
    mapping(uint256 => mapping(bytes32 => bool)) public recallVoted; // recall => owner
    mapping(bytes32 => uint256) internal openRecall; // keccak(community, who, role) => recall + 1

    mapping(bytes32 => mapping(bytes32 => uint64)) public admittedAt; // community => name
    mapping(bytes32 => mapping(bytes32 => bool)) public counted; // community => name: counts in `members`
    mapping(bytes32 => mapping(bytes32 => uint256)) public banAction; // community => name => its ban's action + 1
    mapping(bytes32 => mapping(bytes32 => uint64)) public followFrom; // community => list => when its bans apply here

    /// A commission: a spend paid into a Bank lock from the treasury (escrow
    /// for hired work), on the condition and demand the admin proposed.
    struct Commission {
        address condition;
        uint64 refundAfter;
        uint256 lock; // the Bank lock + 1, once opened
        bytes demand;
    }
    mapping(uint256 => Commission) public commissions; // action => its lock's terms

    /// Humans only: a community whose members (and voters) must be citizens of
    /// this house, by their owner name; agents read, they don't post or vote.
    mapping(bytes32 => address) public citizensOf; // community => citizens' house (0: anyone)
    mapping(bytes32 => mapping(bytes32 => uint32)) public bannedNamesOf; // community => owner => its names banned here
    mapping(bytes32 => mapping(bytes32 => bool)) public followOwners; // community => list: an owner any of whose names that list bans is kept out
    /// Moderation claims this contract issued for its actions (CommonsVotes.attestAction): action => EAS uid.
    mapping(uint256 => bytes32) public moderationClaim;

    event Founded(bytes32 indexed community, string name, bytes32 founder);
    event Joined(bytes32 indexed community, bytes32 indexed member);
    event Left(bytes32 indexed community, bytes32 indexed member);
    event Confirmed(bytes32 indexed community, bytes32 indexed member);
    event Acted(uint256 indexed action, bytes32 indexed community, Kind kind, bytes32 by, bytes32 target, uint256 value, string note);
    event Vetoed(uint256 indexed action, bytes32 indexed community, bytes32 indexed by, uint32 vetoes);
    event Reversed(uint256 indexed action, bytes32 indexed community);
    event Paid(uint256 indexed action, bytes32 indexed community, bytes32 to, uint256 amount);
    event Commissioned(uint256 indexed action, bytes32 indexed community, uint256 indexed lock);
    event ElectionOpened(uint256 indexed election, bytes32 indexed community, uint8 role, uint8 seats, uint64 ends);
    event Nominated(uint256 indexed election, bytes32 indexed candidate);
    event Voted(uint256 indexed election, bytes32 indexed voterOwner);
    event ElectionClosed(uint256 indexed election, bytes32 indexed community, uint32 turnout, bool quorum);
    event Elected(uint256 indexed election, bytes32 indexed community, bytes32 indexed who, uint8 role, uint64 until);
    event RecallOpened(uint256 indexed recall, bytes32 indexed community, bytes32 indexed who, uint8 role);
    event RecallSigned(uint256 indexed recall, bytes32 indexed by, uint32 signatures);
    event RecallVoting(uint256 indexed recall, bytes32 indexed community, bytes32 indexed who, uint64 ends);
    event RecallVoted(uint256 indexed recall, bytes32 indexed by);
    event CitizensOnly(bytes32 indexed community, address house);
    event RecallClosed(uint256 indexed recall, bytes32 indexed community, bytes32 indexed who, bool removed, uint32 yes, uint32 no);

    constructor(Registry r, Sets s, IBankPay b) {
        registry = r;
        sets = s;
        bank = b;
    }

    /// The caller acts for `who`; returns who's owner (the voting unit) and
    /// whether the caller is an agent's key.
    function _as(bytes32 who) internal view returns (bytes32 owner, bool isAgent) {
        bool ok;
        (ok, owner, isAgent) = registry.canAct(who, msg.sender);
        require(ok, "not yours");
    }

    /// A member who may vote here:
    ///  - banned here (it or its owner) means no vote; bans in lists this
    ///    community follows never take a vote away (CM-3): they only keep names out;
    ///  - a member banned by an action whose veto window is still open keeps its
    ///    veto (CM-1), so a mass ban can't remove the people who would reverse it;
    ///  - a new member votes once it has been a member for one veto window,
    ///    except those admitted in the community's founding window (CM-2);
    ///  - an agent's key only where agents vote.
    /// The first vote of a member counts it in `members` (or anyone calls confirm).
    function _voter(bytes32 c, bytes32 who, bool vetoing) internal returns (bytes32 owner) {
        bool isAgent;
        (owner, isAgent) = _as(who);
        bool member = isMember[c][who];
        if (member) require(!bannedHere(c, who), "members only");
        else require(vetoing && banPending(c, who), "members only");
        if (isAgent) require(communities[c].p.agentsVote && citizensOf[c] == address(0), "agents don't vote here");
        _citizen(c, owner);
        require(seasoned(c, who), "a new member votes after one veto window");
        if (member) _count(c, who);
    }

    /// In a citizens-only community: an owner name that is a citizen now.
    function _citizen(bytes32 c, bytes32 owner) internal view {
        address h = citizensOf[c];
        require(h == address(0) || ICitizensHouse(h).isCitizen(owner), "citizens only");
    }

    /// Admitted in the founding window, or at least one veto window ago.
    function seasoned(bytes32 c, bytes32 who) public view returns (bool) {
        Community storage k = communities[c];
        uint64 at = admittedAt[c][who];
        return at < k.created + k.p.vetoWindow || at + k.p.vetoWindow <= block.timestamp;
    }

    /// Banned by a ban of it as a member whose veto window is open.
    function banPending(bytes32 c, bytes32 who) public view returns (bool) {
        uint256 b = banAction[c][who];
        if (b == 0 || !banned[c][who]) return false;
        Action storage a = actions[b - 1];
        return !a.reversed && a.prev != 0 && block.timestamp < a.at + communities[c].p.vetoWindow;
    }

    function _count(bytes32 c, bytes32 who) internal {
        if (counted[c][who]) return;
        counted[c][who] = true;
        if (namesOfOwner[c][registry.ownerOf(who)]++ == 0) communities[c].members++;
        emit Confirmed(c, who);
    }

    function _uncount(bytes32 c, bytes32 who) internal {
        if (!counted[c][who]) return;
        counted[c][who] = false;
        if (--namesOfOwner[c][registry.ownerOf(who)] == 0) communities[c].members--;
    }

    function _bpsOf(uint256 members, uint16 bps) internal pure returns (uint32) {
        uint256 n = members * bps;
        return uint32(n / 10000 + (n % 10000 == 0 ? 0 : 1));
    }

    function _bps(bytes32 c, uint16 bps) internal view returns (uint32) {
        return _bpsOf(communities[c].members, bps);
    }

    function hasRole(bytes32 c, bytes32 who, uint8 role) public view returns (bool) {
        return roleUntil[c][who][role] > block.timestamp || (role == MOD && roleUntil[c][who][ADMIN] > block.timestamp);
    }

    /// Banned here (it or its owner).
    function bannedHere(bytes32 c, bytes32 who) public view returns (bool) {
        return banned[c][who] || banned[c][registry.ownerOf(who)];
    }

    /// A ban in list l that its members could no longer veto: a followed list's
    /// new bans reach its followers only after the list's veto window.
    function _settled(bytes32 l, bytes32 who) internal view returns (bool) {
        if (!banned[l][who]) return false;
        uint256 b = banAction[l][who];
        return b == 0 || actions[b - 1].at + communities[l].p.vetoWindow <= block.timestamp;
    }

    /// Banned here or by a community this one follows (once that follow applies,
    /// and once the list's ban is past its own veto window);
    /// a banned owner's agents are banned too. This keeps names out; votes use bannedHere.
    function isBanned(bytes32 c, bytes32 who) public view returns (bool) {
        if (bannedHere(c, who)) return true;
        bytes32 o = registry.ownerOf(who);
        bytes32[] storage f = following[c];
        for (uint256 i = 0; i < f.length; i++) {
            if (followFrom[c][f[i]] <= block.timestamp
                && (_settled(f[i], who) || _settled(f[i], o) || followOwners[c][f[i]] && bannedNamesOf[f[i]][o] > 0)) return true;
        }
        return false;
    }
}
