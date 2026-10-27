// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./CommonsBase.sol";
import {IEAS, AttestationRequest, AttestationRequestData, RevocationRequest, RevocationRequestData}
    from "@ethereum-attestation-service/eas-contracts/contracts/IEAS.sol";

/// Elections and recalls of Commons, and Commons as an ICommunity (Community.sol)
/// with its moderation claims. Its code runs on Commons' storage
/// (Commons delegates every call it doesn't know here), so call these on the
/// Commons address; called directly, this contract only sees its own empty storage.
contract CommonsVotes is CommonsBase {
    IEAS public immutable eas; // moderation claims (MODERATION_SCHEMA), issued by Commons itself

    constructor(Registry r, Sets s, IBankPay b, IEAS e) CommonsBase(r, s, b) {
        eas = e;
    }

    // ------------------------------------------------------------ ICommunity (isMember, isBanned: CommonsBase)

    /// keccak256("admin") or keccak256("moderator") (an admin moderates too).
    function holdsRole(bytes32 c, bytes32 role, bytes32 who) external view returns (bool) {
        if (role == keccak256("admin")) return hasRole(c, who, ADMIN);
        return role == keccak256("moderator") && hasRole(c, who, MOD);
    }

    /// A moderator or admin now: not suspended by a recall, a citizen where citizens only.
    function canModerate(bytes32 c, bytes32 who) public view returns (bool) {
        address h = citizensOf[c];
        return hasRole(c, who, MOD) && block.timestamp >= suspendedUntil[c][who]
            && (h == address(0) || ICitizensHouse(h).isCitizen(registry.ownerOf(who)));
    }

    function supportsInterface(bytes4 i) external pure returns (bool) {
        return i == type(ICommunity).interfaceId || i == 0x01ffc9a7;
    }

    function moderationSchema() public pure returns (bytes32) {
        return keccak256(abi.encodePacked(MODERATION_SCHEMA, address(0), true));
    }

    /// Anyone mirrors a ban, unban or hide into a moderation claim attested by
    /// this contract (MODERATION_SCHEMA), so any client following moderation
    /// claims sees it; once.
    function attestAction(uint256 id) external returns (bytes32 uid) {
        Action storage a = actions[id];
        require(!a.reversed && moderationClaim[id] == 0, "reversed, or attested already");
        uint8 m = a.kind == Kind.Ban ? MOD_BAN : a.kind == Kind.Unban ? MOD_UNBAN : a.kind == Kind.Hide ? MOD_HIDE : 0;
        require(m != 0, "not a moderation action");
        uid = eas.attest(AttestationRequest(moderationSchema(), AttestationRequestData(address(0), 0, true, 0,
            abi.encode(address(this), a.community, m, a.target, a.by, a.note), 0)));
        moderationClaim[id] = uid;
    }

    /// Anyone revokes the claim of an action members reversed.
    function revokeAction(uint256 id) external {
        bytes32 uid = moderationClaim[id];
        require(uid != 0 && actions[id].reversed, "not reversed, or no claim");
        moderationClaim[id] = bytes32(uint256(1)); // spent: never attested again
        eas.revoke(RevocationRequest(moderationSchema(), RevocationRequestData(uid, 0)));
    }

    // ------------------------------------------------------------ humans only

    /// Makes a new community citizens-only, for good: its members must be
    /// citizens of `house` (their owner names; agents read but don't join, post
    /// or vote). Only its founding admin, a citizen, in the founding window,
    /// before anyone else joined.
    function citizensOnly(bytes32 c, bytes32 by, address house) external {
        _as(by);
        Community storage k = communities[c];
        // members == 1 counts owners: namesOfOwner keeps out the founder's own agents too (HM-4; in the
        // founding window every admitted name is counted)
        require(hasRole(c, by, ADMIN) && block.timestamp < k.created + k.p.vetoWindow && k.members == 1 && namesOfOwner[c][by] == 1
            && citizensOf[c] == address(0) && house.code.length > 0, "only a new community's founder, before others join");
        citizensOf[c] = house;
        _citizen(c, by);
        emit CitizensOnly(c, house);
    }

    // ------------------------------------------------------------ elections

    /// Any member may call an election for a role. The winners hold it for a term
    /// from the close; holders not re-elected keep theirs until their own term ends.
    function callElection(bytes32 c, bytes32 by, uint8 role, uint8 seats) external returns (uint256 e) {
        _voter(c, by, false);
        require((role == ADMIN || role == MOD) && seats > 0 && seats <= 9, "role or seats");
        e = elections.length;
        elections.push();
        Election storage x = elections[e];
        x.community = c;
        x.role = role;
        x.seats = seats;
        x.ends = uint64(block.timestamp) + communities[c].p.voteWindow;
        emit ElectionOpened(e, c, role, seats, x.ends);
    }

    /// Stand yourself (only you can put your name up).
    function stand(uint256 e, bytes32 who) external {
        Election storage x = elections[e];
        (bytes32 o,) = _as(who);
        require(isMember[x.community][who] && !bannedHere(x.community, who), "members only");
        _citizen(x.community, o); // HM-15
        require(block.timestamp < x.ends && x.candidates.length < 32, "closed");
        for (uint256 i = 0; i < x.candidates.length; i++) require(x.candidates[i] != who, "standing");
        x.candidates.push(who);
        emit Nominated(e, who);
    }

    function vote(uint256 e, bytes32 who, bytes32 candidate) external {
        Election storage x = elections[e];
        bytes32 o = _voter(x.community, who, false);
        require(block.timestamp < x.ends, "closed");
        require(!votedIn[e][o], "voted");
        bool found_;
        for (uint256 i = 0; i < x.candidates.length; i++) if (x.candidates[i] == candidate) found_ = true;
        require(found_, "not a candidate");
        votedIn[e][o] = true;
        tally[e][candidate]++;
        x.turnout++;
        emit Voted(e, o);
    }

    /// Closes an election: without quorum (0 turnout included) nobody is elected.
    /// A seat needs more than turnout / (seats + 1) votes (the Droop quota, CM-4),
    /// so one vote can't win one of nine seats; a winner must still be a member.
    function closeElection(uint256 e) external {
        Election storage x = elections[e];
        require(block.timestamp >= x.ends && !x.closed, "not yet");
        x.closed = true;
        bytes32 c = x.community;
        bool q = x.turnout >= _bps(c, communities[c].p.quorumBps);
        emit ElectionClosed(e, c, x.turnout, q);
        if (!q) return;
        bytes32[] memory cs = x.candidates;
        uint64 until = uint64(block.timestamp) + communities[c].p.term;
        uint32 quota = x.turnout / (uint32(x.seats) + 1) + 1;
        for (uint256 s = 0; s < x.seats; s++) {
            uint256 best = type(uint256).max;
            uint32 top;
            for (uint256 i = 0; i < cs.length; i++) {
                if (cs[i] == 0) continue;
                uint32 t = tally[e][cs[i]];
                if (t > top) { // ties: the earlier candidate
                    top = t;
                    best = i;
                }
            }
            if (best == type(uint256).max || top < quota) break;
            address h = citizensOf[c]; // citizens only: a winner who is no longer a citizen isn't seated (HM-15)
            if (isMember[c][cs[best]] && !bannedHere(c, cs[best])
                && (h == address(0) || ICitizensHouse(h).isCitizen(registry.ownerOf(cs[best])))) {
                roleUntil[c][cs[best]][x.role] = until;
                emit Elected(e, c, cs[best], x.role, until);
            }
            cs[best] = 0;
        }
    }

    function electionOf(uint256 e) external view returns (Election memory x) {
        return elections[e];
    }

    function electionCount() external view returns (uint256) {
        return elections.length;
    }

    // ------------------------------------------------------------ recall

    /// Opens a petition to recall `who` from `role` (one open petition per role
    /// holder: sign that one), with the opener's signature. Signatures must come
    /// within voteWindow.
    function petition(bytes32 c, bytes32 by, bytes32 who, uint8 role) external returns (uint256 r) {
        bytes32 o = _voter(c, by, false);
        require((role == ADMIN || role == MOD) && roleUntil[c][who][role] > block.timestamp, "doesn't hold that role");
        bytes32 key = keccak256(abi.encode(c, who, role));
        uint256 prev = openRecall[key];
        require(prev == 0 || !_open(recalls[prev - 1]), "a petition is open: sign it");
        r = recalls.length;
        recalls.push(Recall(c, who, role, uint64(block.timestamp), 0, 0, 0, 0, false));
        openRecall[key] = r + 1;
        emit RecallOpened(r, c, who, role);
        _sign(r, o);
    }

    function _open(Recall storage x) internal view returns (bool) {
        if (x.closed) return false;
        if (x.ends == 0) return block.timestamp < x.opened + communities[x.community].p.voteWindow;
        return block.timestamp < x.ends;
    }

    function sign(uint256 r, bytes32 by) external {
        bytes32 o = _voter(recalls[r].community, by, false);
        _sign(r, o);
    }

    function _sign(uint256 r, bytes32 o) internal {
        Recall storage x = recalls[r];
        Community storage k = communities[x.community];
        require(x.ends == 0 && !x.closed && block.timestamp < x.opened + k.p.voteWindow, "petition over");
        require(!signed[r][o], "signed");
        signed[r][o] = true;
        x.signatures++;
        emit RecallSigned(r, o, x.signatures);
        if (x.signatures >= _bps(x.community, k.p.petitionBps)) {
            x.ends = uint64(block.timestamp) + k.p.voteWindow;
            if (suspendedUntil[x.community][x.who] < x.ends) suspendedUntil[x.community][x.who] = x.ends;
            emit RecallVoting(r, x.community, x.who, x.ends);
        }
    }

    function voteRecall(uint256 r, bytes32 by, bool remove) external {
        Recall storage x = recalls[r];
        bytes32 o = _voter(x.community, by, false);
        require(x.ends != 0 && block.timestamp < x.ends && !x.closed, "not voting");
        require(!recallVoted[r][o], "voted");
        recallVoted[r][o] = true;
        if (remove) x.yes++;
        else x.no++;
        emit RecallVoted(r, o);
    }

    /// After the vote: removed with quorum and more yes than no. The term ends
    /// now, whatever the holder did meanwhile (re-election included).
    function closeRecall(uint256 r) external {
        Recall storage x = recalls[r];
        require(x.ends != 0 && block.timestamp >= x.ends && !x.closed, "not yet");
        x.closed = true;
        bytes32 c = x.community;
        bool removed = x.yes + x.no >= _bps(c, communities[c].p.quorumBps) && x.yes > x.no;
        if (removed) {
            delete roleUntil[c][x.who][x.role];
            if (x.role == ADMIN) delete roleUntil[c][x.who][MOD];
        }
        if (suspendedUntil[c][x.who] == x.ends) delete suspendedUntil[c][x.who];
        emit RecallClosed(r, c, x.who, removed, x.yes, x.no);
    }

    function recallCount() external view returns (uint256) {
        return recalls.length;
    }
}
