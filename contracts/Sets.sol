// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import "./Community.sol";

/// A set of members with roles and a join rule, plus a slot for public data.
/// That is all. A group chat is a set plus a channel (keys, off chain); a
/// paid club is a set plus a subscription; a game team, a DAO, an
/// allowlist, a city forum are sets with different roles and rules.
/// Moderation is done with attestations from members holding a role
/// (MODERATION_SCHEMA, Community.sol): a set is an ICommunity.
///
/// The join rule is the enum (open, approval, closed) plus an optional hook:
/// a set's admins name any IJoinRule contract (token-gated, citizen-gated,
/// paid, invite codes). With a hook, a request it accepts joins at once; one it
/// refuses waits for an admin in an approval set and is refused otherwise.
contract Sets is ICommunity {
    Registry public immutable registry;

    enum Join { Open, Approval, Closed }

    struct Set {
        bytes32 owner;
        Join rule;
        bool exists;
        string name;
    }

    mapping(bytes32 => Set) public sets;
    mapping(bytes32 => mapping(bytes32 => bool)) public members;
    mapping(bytes32 => mapping(bytes32 => bool)) public blocked;
    mapping(bytes32 => mapping(bytes32 => mapping(bytes32 => bool))) public roles; // set => keccak(role) => member
    mapping(bytes32 => mapping(bytes32 => bool)) public requested; // set => member: asked to join, not yet added
    mapping(bytes32 => address) public joinRule; // set => its IJoinRule (0: the enum alone)

    event Created(bytes32 indexed id, string name, bytes32 owner, uint8 rule);
    event Requested(bytes32 indexed id, bytes32 indexed member);
    event Added(bytes32 indexed id, bytes32 indexed member, bytes32 by);
    event Removed(bytes32 indexed id, bytes32 indexed member, bytes32 by, bool blocked, string reason);
    event RoleSet(bytes32 indexed id, string role, bytes32 indexed member, bool on);
    event Published(bytes32 indexed id, string key, bytes value, bytes32 by);
    event JoinRuleSet(bytes32 indexed id, address rule, bytes32 by);

    constructor(Registry r) {
        registry = r;
    }

    function act(bytes32 who) internal view {
        (bool ok,,) = registry.canAct(who, msg.sender);
        require(ok, "not yours");
    }

    function idOf(string memory name) public pure returns (bytes32) {
        return keccak256(bytes(string.concat("set:", name)));
    }

    /// The owner holds every role.
    function hasRole(bytes32 id, string memory role, bytes32 who) public view returns (bool) {
        return sets[id].owner == who || roles[id][keccak256(bytes(role))][who];
    }

    function create(string calldata name, bytes32 owner, uint8 rule) external returns (bytes32 id) {
        act(owner);
        require(rule <= uint8(Join.Closed), "rule");
        require(bytes(name).length > 0 && bytes(name).length <= 64, "name");
        id = idOf(name);
        require(!sets[id].exists, "taken");
        sets[id] = Set(owner, Join(rule), true, name);
        members[id][owner] = true;
        emit Created(id, name, owner, rule);
        emit Added(id, owner, owner);
    }

    function request(bytes32 id, bytes32 member) external {
        _request(id, member, "");
    }

    /// A request with something for the set's join rule (a proof, an invite).
    function requestWith(bytes32 id, bytes32 member, bytes calldata data) external {
        _request(id, member, data);
    }

    function _request(bytes32 id, bytes32 member, bytes memory data) internal {
        act(member);
        Set storage s = sets[id];
        require(s.exists && !blocked[id][member] && !members[id][member], "cannot join");
        address h = joinRule[id];
        if (h != address(0)) {
            if (IJoinRule(h).canJoin(id, member, data)) {
                members[id][member] = true;
                requested[id][member] = false;
                emit Added(id, member, member);
                return;
            }
            require(s.rule == Join.Approval, "the set's join rule refuses");
        }
        require(s.rule != Join.Closed, "closed");
        if (s.rule == Join.Open) {
            members[id][member] = true;
            emit Added(id, member, member);
        } else {
            requested[id][member] = true;
            emit Requested(id, member);
        }
    }

    function add(bytes32 id, bytes32 by, bytes32 member) external {
        act(by);
        require(hasRole(id, "admin", by) && !blocked[id][member], "not allowed");
        members[id][member] = true;
        requested[id][member] = false;
        emit Added(id, member, by);
    }

    /// Leave (by the member), or remove (by an admin or moderator), optionally blocking a return.
    function remove(bytes32 id, bytes32 by, bytes32 member, bool block_, string calldata reason) external {
        act(by);
        bool self = by == member;
        require(self || hasRole(id, "admin", by) || hasRole(id, "moderator", by), "not allowed");
        require(member != sets[id].owner, "the owner stays");
        members[id][member] = false;
        requested[id][member] = false;
        roles[id][keccak256("admin")][member] = false;
        roles[id][keccak256("moderator")][member] = false;
        if (block_ && !self) blocked[id][member] = true;
        if (!block_ && !self) blocked[id][member] = false;
        emit Removed(id, member, by, block_ && !self, reason);
    }

    function setRole(bytes32 id, bytes32 by, string calldata role, bytes32 member, bool on) external {
        act(by);
        require(sets[id].owner == by || hasRole(id, "admin", by), "admins only");
        roles[id][keccak256(bytes(role))][member] = on;
        emit RoleSet(id, role, member, on);
    }

    /// Names the set's join rule (0: none). Admins only.
    function setJoinRule(bytes32 id, bytes32 by, address rule) external {
        act(by);
        require(hasRole(id, "admin", by), "admins only");
        require(rule == address(0) || rule.code.length > 0, "a contract");
        joinRule[id] = rule;
        emit JoinRuleSet(id, rule, by);
    }

    // ---- ICommunity

    function isMember(bytes32 id, bytes32 who) external view returns (bool) {
        return members[id][who];
    }

    function isBanned(bytes32 id, bytes32 who) external view returns (bool) {
        return blocked[id][who];
    }

    function holdsRole(bytes32 id, bytes32 role, bytes32 who) external view returns (bool) {
        return sets[id].owner == who || roles[id][role][who];
    }

    function canModerate(bytes32 id, bytes32 who) external view returns (bool) {
        return hasRole(id, "admin", who) || hasRole(id, "moderator", who);
    }

    function supportsInterface(bytes4 i) external pure returns (bool) {
        return i == type(ICommunity).interfaceId || i == 0x01ffc9a7;
    }

    /// Public data for the set: channel keys of a public chat, rules, a logo hash, prices.
    function publish(bytes32 id, bytes32 by, string calldata key, bytes calldata value) external {
        act(by);
        require(hasRole(id, "admin", by), "admins only");
        require(value.length <= 64 * 1024, "too big");
        emit Published(id, key, value, by);
    }
}
