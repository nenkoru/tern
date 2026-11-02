// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./CommonsBase.sol";

/// Communities that govern themselves: no fixed authorities.
///
/// A community is a name owned by this contract plus a set (Sets) that only
/// this contract manages. Its members (people and agents; one vote per
/// owner, so an owner's ten agents count once) elect admins and moderators
/// for a term. Elected roles have real powers over the community:
///
///   admins      admit, ban, unban, hide, rules, posting fee, treasury spends and commissions (escrowed spends)
///               within a budget, blocklists to follow
///   moderators  admit, ban, hide
///
/// Every power is answerable: an action takes effect at once (a treasury
/// spend waits out the window, a follow applies after it), and members may
/// veto it within the window; enough vetoes (a share of the voting members
/// when the action was taken) reverse it. A reversed ban restores the member
/// as it was, roles included; a member under a ban whose window is open keeps
/// its veto. A ban that stood can be lifted by an admin's unban (vetoable too).
/// A petition of enough members opens a recall vote on anyone holding a role;
/// while it runs, that role holder's powers are suspended. Roles end with
/// their term; the founder's first term is the same as anyone's.
///
/// New members vote (and count toward thresholds) only after one veto window,
/// so a moderator can't flood the electorate before members can veto the
/// admissions. Members admitted in the community's founding window vote at once:
/// there is nobody yet to protect.
///
/// Bans are public, so communities can follow each other's bans (shared
/// blocklists), and so can people and agents in their own clients: a group
/// of humans and agents can block a misbehaving name for everyone who opts in.
/// A ban of an owner's name covers its agents. A followed list keeps names out
/// of this community; it never takes a vote from a member here.
///
/// Elections and recalls live in CommonsVotes, whose code runs here (fallback).
contract Commons is CommonsBase {
    address private constant NOBODY = address(1); // the community name's guardian: no key can act as it
    address public immutable votes; // CommonsVotes: elections and recalls, on this contract's storage

    constructor(Registry r, Sets s, IBankPay b, address v) CommonsBase(r, s, b) {
        votes = v;
    }

    /// Elections and recalls: CommonsVotes' code on this storage.
    fallback() external {
        address v = votes;
        assembly {
            calldatacopy(0, 0, calldatasize())
            let ok := delegatecall(gas(), v, 0, calldatasize(), 0, 0)
            returndatacopy(0, 0, returndatasize())
            switch ok
            case 0 { revert(0, returndatasize()) }
            default { return(0, returndatasize()) }
        }
    }

    function _founding(bytes32 c) internal view returns (bool) {
        return block.timestamp < communities[c].created + communities[c].p.vetoWindow;
    }

    function followingOf(bytes32 c) external view returns (bytes32[] memory) {
        return following[c];
    }

    // ------------------------------------------------------------ founding and membership

    /// Founds a community: a new name owned by this contract and its set. The
    /// founder is its first admin, for one term, like anyone elected.
    function found(string calldata name, bytes32 founder, Params calldata p, string calldata rules) external returns (bytes32 c) {
        _as(founder);
        require(p.term > 0 && p.voteWindow > 0 && p.vetoWindow > 0 && p.vetoBps > 0 && p.vetoBps <= 10000
            && p.petitionBps > 0 && p.petitionBps <= 10000 && p.quorumBps > 0 && p.quorumBps <= 10000, "params");
        c = registry.register(name, NOBODY);
        bytes32 set = sets.create(name, c, 1); // approval: only this contract admits
        Community storage k = communities[c];
        k.id = c;
        k.set = set;
        k.created = uint64(block.timestamp);
        k.rules = rules;
        k.p = p;
        k.budgetFrom = uint64(block.timestamp);
        emit Founded(c, name, founder);
        _admit(c, founder);
        roleUntil[c][founder][ADMIN] = uint64(block.timestamp) + p.term;
        emit Elected(type(uint256).max, c, founder, ADMIN, uint64(block.timestamp) + p.term);
    }

    function _admit(bytes32 c, bytes32 who) internal {
        require(!isBanned(c, who), "banned");
        require(!isMember[c][who], "already a member");
        if (citizensOf[c] != address(0)) {
            require(registry.ownerOf(who) == who, "citizens only: agents read, they don't join");
            _citizen(c, who);
        }
        isMember[c][who] = true;
        admittedAt[c][who] = uint64(block.timestamp);
        if (_founding(c)) _count(c, who);
        sets.add(communities[c].set, c, who);
        emit Joined(c, who);
    }

    /// Counts a member that has waited out its veto window (its first vote does this too).
    function confirm(bytes32 c, bytes32 who) external {
        require(isMember[c][who] && !bannedHere(c, who) && seasoned(c, who), "not yet");
        _count(c, who);
    }

    function _drop(bytes32 c, bytes32 who, bool block_, string memory note) internal {
        if (!isMember[c][who]) return;
        isMember[c][who] = false;
        _uncount(c, who);
        delete roleUntil[c][who][ADMIN];
        delete roleUntil[c][who][MOD];
        sets.remove(communities[c].set, c, who, block_, note);
        emit Left(c, who);
    }

    /// Leave a community (your own name).
    function leave(bytes32 c, bytes32 who) external {
        _as(who);
        _drop(c, who, false, "left");
    }

    // ------------------------------------------------------------ powers (answerable)

    function _act(bytes32 c, bytes32 by, uint8 needs, Kind kind, bytes32 target, uint256 value, bytes32 prev, string memory note)
        internal returns (uint256 id) {
        (bytes32 o,) = _as(by);
        require(communities[c].created != 0, "no community");
        _citizen(c, o); // citizens only: a role holder who is no longer a citizen has no powers (HM-15)
        require(hasRole(c, by, needs), needs == ADMIN ? "admins only" : "moderators only");
        require(block.timestamp >= suspendedUntil[c][by], "suspended while a recall vote on you runs");
        id = actions.length;
        actions.push(Action(c, kind, by, target, value, prev, note, uint64(block.timestamp), 0, false, false, communities[c].members));
        emit Acted(id, c, kind, by, target, value, note);
    }

    /// Admits a name that asked to join (Sets.request): nobody is put in a community without consent.
    function admit(bytes32 c, bytes32 by, bytes32 who) external returns (uint256 id) {
        require(sets.requested(communities[c].set, who), "no join request from that name");
        id = _act(c, by, MOD, Kind.Admit, who, 0, 0, "");
        _admit(c, who);
    }

    /// Bans a name (a member or not) here: it leaves, and can't come back unless
    /// the ban is reversed or lifted. A member's roles and admission are kept in
    /// the action, so a reversal restores them; until the window closes the
    /// banned member keeps its veto (not on its own ban).
    function ban(bytes32 c, bytes32 by, bytes32 who, string calldata reason) external returns (uint256 id) {
        require(who != c && !banned[c][who], "already banned");
        require(!hasRole(c, who, ADMIN) || hasRole(c, by, ADMIN), "a moderator can't ban an admin");
        bool was = isMember[c][who];
        uint256 state = was ? uint256(roleUntil[c][who][ADMIN]) | uint256(roleUntil[c][who][MOD]) << 64
            | uint256(admittedAt[c][who]) << 128 | (counted[c][who] ? 1 << 192 : 0) : 0;
        id = _act(c, by, MOD, Kind.Ban, who, state, was ? bytes32(uint256(1)) : bytes32(0), reason);
        _setBan(c, who, true);
        banAction[c][who] = id + 1;
        if (was) _drop(c, who, true, reason);
        else sets.remove(communities[c].set, c, who, true, reason); // blocked in the set too
    }

    /// A ban here, counted per owner too (lists followed owner-wide keep out all its names).
    function _setBan(bytes32 c, bytes32 who, bool on) internal {
        if (banned[c][who] == on) return;
        banned[c][who] = on;
        if (on) bannedNamesOf[c][registry.ownerOf(who)]++;
        else bannedNamesOf[c][registry.ownerOf(who)]--;
    }

    /// Lifts a ban (after its veto window: within it, veto the ban). The name
    /// may ask to join again. Vetoable: a reversal bans it again.
    function unban(bytes32 c, bytes32 by, bytes32 who, string calldata reason) external returns (uint256 id) {
        require(banned[c][who], "not banned");
        uint256 b = banAction[c][who];
        require(b == 0 || block.timestamp >= actions[b - 1].at + communities[c].p.vetoWindow, "the ban can still be vetoed");
        id = _act(c, by, ADMIN, Kind.Unban, who, 0, 0, reason);
        _setBan(c, who, false);
        sets.remove(communities[c].set, c, who, false, reason); // clears the block in Sets
    }

    /// Hides a post, a thread, an artifact or a name's posts (`ref` is whatever the clients agree on).
    function hide(bytes32 c, bytes32 by, bytes32 ref, string calldata reason) external returns (uint256 id) {
        id = _act(c, by, MOD, Kind.Hide, ref, 0, 0, reason);
        hidden[c][ref] = true;
    }

    function setRules(bytes32 c, bytes32 by, string calldata rules) external returns (uint256 id) {
        id = _act(c, by, ADMIN, Kind.Rules, keccak256(bytes(rules)), 0, keccak256(bytes(communities[c].rules)), communities[c].rules);
        communities[c].rules = rules;
    }

    function setPostFee(bytes32 c, bytes32 by, uint128 fee) external returns (uint256 id) {
        id = _act(c, by, ADMIN, Kind.Fee, 0, fee, bytes32(uint256(communities[c].postFee)), "");
        communities[c].postFee = fee;
    }

    /// Follow (or stop following) another community's bans. mode 0: stop; 1: its
    /// banned names; 2: owner-wide, also every name of an owner any of whose
    /// names (an agent) it bans. A new follow applies after the veto window (at
    /// once in the founding window); it keeps banned names out of this community
    /// and never takes a member's vote.
    function follow(bytes32 c, bytes32 by, bytes32 list, uint8 mode) external returns (uint256 id) {
        require(list != c && mode <= 2 && (mode == 0 || communities[list].created != 0), "not a community");
        uint256 was = followFrom[c][list] == 0 ? 0 : followOwners[c][list] ? 2 : 1;
        id = _act(c, by, ADMIN, Kind.Follow, list, mode, bytes32(was), "");
        require(_follow(c, list, mode, _founding(c) ? block.timestamp : block.timestamp + communities[c].p.vetoWindow),
            "follows at most 8 lists");
    }

    function _follow(bytes32 c, bytes32 list, uint256 mode, uint256 from) internal returns (bool) {
        bool on = mode != 0;
        followOwners[c][list] = mode == 2;
        bytes32[] storage f = following[c];
        for (uint256 i = 0; i < f.length; i++) {
            if (f[i] == list) {
                if (!on) {
                    f[i] = f[f.length - 1];
                    f.pop();
                    delete followFrom[c][list];
                }
                return true;
            }
        }
        if (!on) return true;
        if (f.length >= 8) return false;
        f.push(list);
        followFrom[c][list] = uint64(from);
        return true;
    }

    /// A treasury spend: within the budget per term, paid only after the veto window.
    function spend(bytes32 c, bytes32 by, bytes32 to, uint128 amount, string calldata memo) external returns (uint256 id) {
        id = _spend(c, by, Kind.Spend, to, amount, memo);
    }

    /// Hiring through an escrow: after the veto window, payOut opens a Bank
    /// lock from the community's name to `to` under `condition` and `demand`
    /// (a panel of members, an arbiter's claim), refundable to the treasury
    /// after refundAfter. Counts against the budget like a spend.
    function commission(bytes32 c, bytes32 by, bytes32 to, uint128 amount, uint64 refundAfter, address condition,
        bytes calldata demand, string calldata terms) external returns (uint256 id) {
        require(refundAfter > block.timestamp + communities[c].p.vetoWindow && condition.code.length > 0, "refund time or condition");
        id = _spend(c, by, Kind.Commission, to, amount, terms);
        commissions[id] = Commission(condition, refundAfter, 0, demand);
    }

    function _spend(bytes32 c, bytes32 by, Kind kind, bytes32 to, uint128 amount, string calldata memo) internal returns (uint256) {
        Community storage k = communities[c];
        require(amount > 0 && bank.isPayee(to), "amount or recipient");
        if (block.timestamp >= k.budgetFrom + k.p.term) {
            k.budgetFrom = uint64(block.timestamp);
            k.spentThisTerm = 0;
        }
        require(k.spentThisTerm + amount <= k.p.budget, "over the community's budget");
        k.spentThisTerm += amount;
        return _act(c, by, ADMIN, kind, to, amount, bytes32(uint256(k.budgetFrom)), memo); // prev: the budget period
    }

    /// Anyone pays out a spend (or opens a commission's lock) once its veto
    /// window has passed unreversed, within a term of it (CM-6), and not if its
    /// author has been banned here since.
    function payOut(uint256 id) external {
        Action storage a = actions[id];
        Params storage p = communities[a.community].p;
        require((a.kind == Kind.Spend || a.kind == Kind.Commission) && !a.done && !a.reversed, "nothing to pay");
        require(block.timestamp >= a.at + p.vetoWindow, "veto window open");
        require(block.timestamp < a.at + p.vetoWindow + p.term && !bannedHere(a.community, a.by), "the spend has lapsed");
        a.done = true;
        if (a.kind == Kind.Spend) {
            bank.pay(a.community, a.target, a.value, keccak256(bytes(a.note)));
            emit Paid(id, a.community, a.target, a.value);
            return;
        }
        Commission storage m = commissions[id];
        uint256 l = bank.lock(a.community, a.target, a.value, m.refundAfter, m.condition, m.demand);
        m.lock = l + 1;
        emit Commissioned(id, a.community, l);
    }

    /// A member's veto of an action, within the window. Vetoes from vetoBps of
    /// the voting members when the action was taken reverse it.
    function veto(uint256 id, bytes32 who) external {
        Action storage a = actions[id];
        bytes32 c = a.community;
        bytes32 o = _voter(c, who, true);
        require(block.timestamp < a.at + communities[c].p.vetoWindow, "veto window closed");
        require(!a.reversed && !a.done && !vetoed[id][o], "already");
        require(a.kind != Kind.Ban || registry.ownerOf(a.target) != o, "not your own ban");
        vetoed[id][o] = true;
        a.vetoes++;
        emit Vetoed(id, c, o, a.vetoes);
        if (a.vetoes >= _bpsOf(a.electorate, communities[c].p.vetoBps)) _reverse(id);
    }

    function _reverse(uint256 id) internal {
        Action storage a = actions[id];
        a.reversed = true;
        bytes32 c = a.community;
        bytes32 t = a.target;
        Community storage k = communities[c];
        if (a.kind == Kind.Admit) {
            _drop(c, t, false, "admission vetoed");
        } else if (a.kind == Kind.Ban) {
            _setBan(c, t, false);
            sets.remove(k.set, c, t, false, "ban vetoed"); // clears the block in Sets
            // back as it was, if it was a member and nothing here bans it
            if (a.prev != 0 && !isMember[c][t] && !bannedHere(c, t)) _restore(c, t, a.value);
        } else if (a.kind == Kind.Unban) {
            _setBan(c, t, true);
            if (isMember[c][t]) _drop(c, t, true, "unban vetoed");
            else sets.remove(k.set, c, t, true, "unban vetoed");
        } else if (a.kind == Kind.Hide) {
            hidden[c][t] = false;
        } else if (a.kind == Kind.Rules) {
            if (keccak256(bytes(k.rules)) == t) k.rules = a.note; // only if not changed since (CM-7)
        } else if (a.kind == Kind.Fee) {
            if (k.postFee == a.value) k.postFee = uint128(uint256(a.prev));
        } else if (a.kind == Kind.Spend || a.kind == Kind.Commission) {
            // the budget comes back only within the same period: a new term starts from zero anyway
            if (uint64(uint256(a.prev)) == k.budgetFrom) k.spentThisTerm -= uint128(a.value);
        } else if (a.kind == Kind.Follow) {
            _follow(c, t, uint256(a.prev), block.timestamp); // back to the mode before; a full list just stays as it is
        }
        emit Reversed(id, c);
    }

    /// A member back as it was before a reversed ban: admission time, count, roles.
    function _restore(bytes32 c, bytes32 who, uint256 s) internal {
        isMember[c][who] = true;
        admittedAt[c][who] = uint64(s >> 128);
        if ((s >> 192) & 1 == 1) _count(c, who);
        roleUntil[c][who][ADMIN] = uint64(s);
        roleUntil[c][who][MOD] = uint64(s >> 64);
        sets.add(communities[c].set, c, who);
        emit Joined(c, who);
    }

    // ------------------------------------------------------------ channel keys

    /// Admins publish the community's channel keys (a public forum, read by anyone; members post).
    function publish(bytes32 c, bytes32 by, string calldata key, bytes calldata value) external {
        (bytes32 o,) = _as(by);
        require(hasRole(c, by, ADMIN) && block.timestamp >= suspendedUntil[c][by], "admins only");
        _citizen(c, o);
        sets.publish(communities[c].set, c, key, value);
    }

    function actionCount() external view returns (uint256) {
        return actions.length;
    }
}
