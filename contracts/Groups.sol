// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import "./Poseidon2.sol";
import "./Community.sol";
import {IVoteVerifier} from "./CitizenGroup.sol";

/// Secret ballots for any group: the citizens' Semaphore machinery
/// (CitizenGroup: Semaphore v4's group, a depth-16 Poseidon2 tree, the same
/// vote circuit and verifier) with one tree per group. A group is a set in
/// any membership source, an ICommunity (Sets, Commons, or anyone's
/// contract): group id = keccak256(abi.encode(source, set)). Anyone opens a
/// group's tree; members' leaves follow the source (sync), as Citizens keeps
/// the citizens' tree.
///
/// Identities: one per owner name, for every group (register; a vote's
/// nullifier is H(scope, secret) and every ballot has its own scope, so one
/// identity can't be linked across ballots). Which leaf is whose is public,
/// as in Semaphore: the group is known, the votes are not.
///
/// Ballots: anyone opens one on a group (what counts is the group's choice:
/// its admins open them, or it reads only ballots its own contract opened);
/// the tree's root then is the electorate. A vote proves a leaf in that root
/// and in a recent root (ROOT_GRACE, no removal since: a member who left
/// can't vote with an old root), with a nullifier per ballot; a relayer
/// submits it. The tally is public, the voters are not.
contract GroupTrees {
    uint256 internal constant P = 21888242871839275222246405745257275088548364400416034343698204186575808495617;
    uint256 public constant DEPTH = 16;
    uint256 public constant ROOT_GRACE = 3600;

    Registry public immutable registry;
    IVoteVerifier public immutable verifier;
    address public immutable hasher; // Poseidon2Hasher

    mapping(bytes32 => uint256) public identityOf; // owner name => commitment
    mapping(uint256 => bytes32) public nameOf; // commitment => name

    struct Tree {
        address source; // the ICommunity
        bytes32 set; // the group's id in it
        uint256 root;
        uint256 size; // leaf slots ever used (freed ones are reused first)
        uint256 removals;
    }

    mapping(bytes32 => Tree) public trees; // group => its tree (source 0: not opened)
    mapping(bytes32 => mapping(uint256 => uint256)) internal nodes; // group => level << 32 | index => node
    mapping(bytes32 => mapping(bytes32 => uint256)) public leafOf; // group => name => leaf index + 1
    mapping(bytes32 => mapping(uint256 => uint256)) public pastRoot; // group => replaced root => removals << 64 | when
    /// Leaf slots freed by members who left, reused before new ones: churn
    /// (join, leave, join) can't fill the 2^DEPTH slots (audit 6).
    mapping(bytes32 => uint256[]) internal freeSlots;

    struct Ballot {
        bytes32 group;
        uint256 root; // the electorate: the group's root when it opened
        uint64 closes;
        uint8 options;
        address by;
    }

    Ballot[] internal ballots;
    mapping(uint256 => uint256[]) internal tallies;
    mapping(uint256 => mapping(uint256 => bool)) public nullified; // ballot => nullifier used

    event GroupOpened(bytes32 indexed group, address indexed source, bytes32 indexed set);
    event IdentitySet(bytes32 indexed who, uint256 commitment);
    event MemberSet(bytes32 indexed group, bytes32 indexed who, uint256 index, uint256 commitment, uint256 root); // commitment 0: removed
    event BallotOpened(uint256 indexed ballot, bytes32 indexed group, uint256 root, uint8 options, uint64 closes, address by, string question);
    event SecretVoted(uint256 indexed ballot, uint256 nullifier, uint8 option);

    constructor(Registry r, IVoteVerifier v, address h) {
        registry = r;
        verifier = v;
        hasher = h;
    }

    function groupOf(address source, bytes32 set) public pure returns (bytes32) {
        return keccak256(abi.encode(source, set));
    }

    function open(address source, bytes32 set) external returns (bytes32 g) {
        require(source.code.length > 0, "the source is a contract");
        g = groupOf(source, set);
        require(trees[g].source == address(0), "opened already");
        trees[g] = Tree(source, set, Poseidon2.zero(DEPTH), 0, 0);
        emit GroupOpened(g, source, set);
    }

    /// An owner name's voting identity (H(pk) of a Baby Jubjub key), for every
    /// group; rotating it needs a sync in each group the name is in.
    function register(bytes32 who, uint256 commitment) external {
        (bool ok, bytes32 o, bool agent) = registry.canAct(who, msg.sender);
        require(ok && !agent && o == who, "the owner's own key only");
        require(commitment != 0 && commitment < P && nameOf[commitment] == 0, "commitment");
        uint256 old = identityOf[who];
        if (old != 0) delete nameOf[old];
        identityOf[who] = commitment;
        nameOf[commitment] = who;
        emit IdentitySet(who, commitment);
    }

    /// Anyone brings a name's leaf in step with the source: in while a member with an identity, out otherwise.
    function sync(bytes32 g, bytes32 who) external {
        Tree storage t = trees[g];
        require(t.source != address(0), "no such group");
        uint256 c = identityOf[who];
        uint256 li = leafOf[g][who];
        ICommunity s = ICommunity(t.source);
        if (c != 0 && registry.ownerOf(who) == who && s.isMember(t.set, who) && !s.isBanned(t.set, who)) {
            if (li == 0) {
                uint256[] storage free = freeSlots[g];
                if (free.length > 0) {
                    li = free[free.length - 1] + 1;
                    free.pop();
                } else {
                    require(t.size < 2 ** DEPTH, "the tree is full");
                    li = ++t.size;
                }
                leafOf[g][who] = li;
            } else if (nodes[g][li - 1] == c) {
                return;
            }
            _set(g, who, li - 1, c);
        } else if (li != 0) {
            delete leafOf[g][who];
            freeSlots[g].push(li - 1);
            _set(g, who, li - 1, 0);
        }
    }

    function _node(bytes32 g, uint256 level, uint256 index) internal view returns (uint256 v) {
        v = nodes[g][(level << 32) | index];
        if (v == 0) v = Poseidon2.zero(level);
    }

    function _set(bytes32 g, bytes32 who, uint256 index, uint256 leaf) internal {
        Tree storage t = trees[g];
        nodes[g][index] = leaf;
        uint256 cur = leaf;
        uint256 i = index;
        for (uint256 d = 0; d < DEPTH; d++) {
            cur = i & 1 == 0 ? Poseidon2.compress(hasher, cur, _node(g, d, i + 1)) : Poseidon2.compress(hasher, _node(g, d, i - 1), cur);
            i >>= 1;
            nodes[g][((d + 1) << 32) | i] = cur;
        }
        pastRoot[g][t.root] = (t.removals << 64) | block.timestamp;
        if (leaf == 0) t.removals++;
        t.root = cur;
        emit MemberSet(g, who, index, leaf, cur);
    }

    /// The group's current root, or one replaced within ROOT_GRACE with no removal since.
    function knownRoot(bytes32 g, uint256 r) public view returns (bool) {
        Tree storage t = trees[g];
        if (r == t.root) return true;
        uint256 p = pastRoot[g][r];
        return p != 0 && p >> 64 == t.removals && block.timestamp < uint64(p) + ROOT_GRACE;
    }

    function rootOf(bytes32 g) external view returns (uint256) {
        return trees[g].root;
    }

    // ---- ballots

    function openBallot(bytes32 g, uint8 options, uint64 closes, string calldata question) external returns (uint256 id) {
        require(trees[g].source != address(0), "no such group");
        require(options >= 2 && closes > block.timestamp, "options or time");
        id = ballots.length;
        ballots.push(Ballot(g, trees[g].root, closes, options, msg.sender));
        tallies[id] = new uint256[](options);
        emit BallotOpened(id, g, trees[g].root, options, closes, msg.sender, question);
    }

    /// The ballot's scope: a vote's nullifier is H(scope, secret).
    function scopeOf(uint256 id) public view returns (uint256) {
        return uint256(keccak256(abi.encode("tern-ballot", block.chainid, address(this), id))) >> 8;
    }

    /// A secret vote (submit it through a relayer): the proof shows a leaf in
    /// the ballot's root and in rootNow, nullifier = H(scope, secret), message = the option.
    function vote(uint256 id, uint256 rootNow, uint256 nullifier, uint8 option, bytes calldata proof) external {
        Ballot storage b = ballots[id];
        require(block.timestamp <= b.closes, "the ballot is closed");
        require(option < b.options, "option");
        require(!nullified[id][nullifier], "voted already");
        require(knownRoot(b.group, rootNow), "the group's tree changed: prove again");
        verifier.verifyProof(proof, [b.root, rootNow, nullifier, scopeOf(id), uint256(option)]);
        nullified[id][nullifier] = true;
        tallies[id][option]++;
        emit SecretVoted(id, nullifier, option);
    }

    function ballot(uint256 id) external view returns (Ballot memory b, uint256[] memory tally) {
        return (ballots[id], tallies[id]);
    }

    function ballotCount() external view returns (uint256) {
        return ballots.length;
    }
}
