// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// A community, whatever constitution runs it: who is in, who holds which
/// role, who is banned, and who may moderate. Commons implements it (its
/// elected admins and moderators), Sets implement it (a set's owner and the
/// roles its admins hand out), and so can a Governor, a Safe or any club
/// contract. Clients take hiding, blocklists and labels from any
/// implementation their owner follows, not from Commons alone.
///   community   the community's id inside that contract (a Commons name id, a set id)
///   role        keccak256 of the role's name: "admin", "moderator", or the community's own
interface ICommunity {
    function isMember(bytes32 community, bytes32 who) external view returns (bool);
    function isBanned(bytes32 community, bytes32 who) external view returns (bool);
    function holdsRole(bytes32 community, bytes32 role, bytes32 who) external view returns (bool);
    /// May `who` hide, ban and unban here (and so issue moderation claims for it)?
    function canModerate(bytes32 community, bytes32 who) external view returns (bool);
}

// The moderation-claim standard: one EAS schema any community contract (or
// its moderators) attests with.
//   community   the ICommunity contract
//   id          the community's id in it
//   action      MOD_HIDE, MOD_BAN, MOD_UNBAN, MOD_UNHIDE
//   target      what: a name id (ban, unban) or a ref (hide: a post, thread, artifact)
//   by          the Tern name that acted (a moderator; the actor for a contract's own claim)
//   reason      free text
// A claim counts for a client while it is not revoked and either its attester
// is `community` itself (the contract issued it: Commons does for its own
// actions, CommonsVotes.attestAction), or its attester acts for `by`
// (Registry.canAct) and ICommunity(community).canModerate(id, by) holds.
// The schema is revocable, resolver 0: uid = keccak256(abi.encodePacked(MODERATION_SCHEMA, address(0), true)).
string constant MODERATION_SCHEMA = "address community,bytes32 id,uint8 action,bytes32 target,bytes32 by,string reason";
uint8 constant MOD_HIDE = 1;
uint8 constant MOD_BAN = 2;
uint8 constant MOD_UNBAN = 3;
uint8 constant MOD_UNHIDE = 4;

/// Who may join a set: Sets asks the set's join rule, if it has one
/// (Sets.setJoinRule). A rule is any contract: token-gated, citizen-gated,
/// paid, invite codes. `data` is whatever the rule wants (a proof, an invite).
interface IJoinRule {
    function canJoin(bytes32 set, bytes32 who, bytes calldata data) external returns (bool);
}

interface IIsCitizen {
    function isCitizen(bytes32 id) external view returns (bool);
}

interface IOwnerOf {
    function ownerOf(bytes32 id) external view returns (bytes32);
}

/// A join rule: names whose owner is a citizen (one person, one membership's worth of trust).
contract CitizenJoinRule is IJoinRule {
    IIsCitizen public immutable citizens;
    IOwnerOf public immutable registry;

    constructor(IIsCitizen c, IOwnerOf r) {
        citizens = c;
        registry = r;
    }

    function canJoin(bytes32, bytes32 who, bytes calldata) external view returns (bool) {
        return citizens.isCitizen(registry.ownerOf(who));
    }
}
