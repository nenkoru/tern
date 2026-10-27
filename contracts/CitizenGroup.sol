// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import "./Poseidon2.sol";

interface ICitizenStatus {
    function isCitizen(bytes32 id) external view returns (bool);
}

interface IVoteVerifier {
    function verifyProof(bytes calldata proof, uint256[5] calldata input) external view;
}

/// The citizens' tree for secret votes (Semaphore v4's group, with a
/// fixed-depth Poseidon2 tree instead of a LeanIMT). Each owner name may
/// register one identity commitment, H(pk) of a Baby Jubjub key; its leaf is
/// in the tree exactly while the name is a citizen. Citizens keeps it in
/// step on every admission, lapse, departure and revocation; anyone may
/// sync a name whose status changed by time or by its key (sync).
///
/// Which leaf belongs to which name is public, as in Semaphore: the group is
/// known, the votes are not. A registration is never withdrawn (it can be
/// rotated): Parliament uses its order to keep one vote per citizen across
/// open and secret voting.
///
/// A vote proves its identity in the root when the proposal opened and in a
/// recent root (audit 5 ZK-1): the current one, or one replaced less than
/// ROOT_GRACE ago with no leaf removed since. Rotating an identity or adding
/// a citizen no longer voids proofs in flight; a removal (lapse, departure,
/// revocation) still does, so a removed citizen can't vote with an old root.
contract CitizenGroup {
    uint256 internal constant P = 21888242871839275222246405745257275088548364400416034343698204186575808495617;
    uint256 public constant DEPTH = 16;
    uint256 public constant ROOT_GRACE = 3600;

    address public immutable citizens;
    Registry public immutable registry;
    IVoteVerifier public immutable verifier;
    address public immutable hasher; // Poseidon2Hasher

    uint256 public root;
    uint256 public size; // leaf slots used (a removed leaf leaves a 0)
    uint256 public registrations;
    mapping(uint256 => uint256) internal nodes; // level << 32 | index => node (0: empty)
    mapping(bytes32 => uint256) public identityOf; // name => commitment
    mapping(bytes32 => uint256) public seqOf; // name => its first registration's number (1-based)
    mapping(bytes32 => uint256) public leafOf; // name => leaf index + 1 (0: not in the tree)
    mapping(uint256 => bytes32) public nameOf; // commitment => name
    uint256 public removals; // leaves removed so far
    mapping(uint256 => uint256) public pastRoot; // a replaced root => removals then << 64 | when it was replaced

    event IdentitySet(bytes32 indexed who, uint256 commitment, uint256 seq);
    event MemberSet(bytes32 indexed who, uint256 index, uint256 commitment, uint256 root); // commitment 0: removed

    constructor(Registry r, IVoteVerifier v, address h) {
        citizens = msg.sender;
        hasher = h;
        registry = r;
        verifier = v;
        root = Poseidon2.zero(DEPTH);
    }

    function register(bytes32 who, uint256 commitment) external {
        (bool ok, bytes32 o, bool agent) = registry.canAct(who, msg.sender);
        require(ok && !agent && o == who, "the owner's own key only");
        require(commitment != 0 && commitment < P && nameOf[commitment] == 0, "commitment");
        uint256 old = identityOf[who];
        if (old != 0) delete nameOf[old];
        identityOf[who] = commitment;
        nameOf[commitment] = who;
        if (seqOf[who] == 0) seqOf[who] = ++registrations;
        emit IdentitySet(who, commitment, seqOf[who]);
        _sync(who);
    }

    function sync(bytes32 who) external {
        _sync(who);
    }

    function _sync(bytes32 who) internal {
        uint256 c = identityOf[who];
        uint256 li = leafOf[who];
        if (c != 0 && ICitizenStatus(citizens).isCitizen(who)) {
            if (li == 0) {
                require(size < 2 ** DEPTH, "the tree is full");
                li = ++size;
                leafOf[who] = li;
            } else if (nodes[li - 1] == c) {
                return;
            }
            _set(who, li - 1, c);
        } else if (li != 0) {
            delete leafOf[who];
            _set(who, li - 1, 0);
        }
    }

    function _node(uint256 level, uint256 index) internal view returns (uint256 v) {
        v = nodes[(level << 32) | index];
        if (v == 0) v = Poseidon2.zero(level);
    }

    function _set(bytes32 who, uint256 index, uint256 leaf) internal {
        nodes[index] = leaf;
        uint256 cur = leaf;
        uint256 i = index;
        for (uint256 d = 0; d < DEPTH; d++) {
            cur = i & 1 == 0 ? Poseidon2.compress(hasher, cur, _node(d, i + 1)) : Poseidon2.compress(hasher, _node(d, i - 1), cur);
            i >>= 1;
            nodes[((d + 1) << 32) | i] = cur;
        }
        pastRoot[root] = (removals << 64) | block.timestamp;
        if (leaf == 0) removals++;
        root = cur;
        emit MemberSet(who, index, leaf, cur);
    }

    /// The current root, or one replaced within ROOT_GRACE with no removal since.
    function knownRoot(uint256 r) public view returns (bool) {
        if (r == root) return true;
        uint256 p = pastRoot[r];
        return p != 0 && p >> 64 == removals && block.timestamp < uint64(p) + ROOT_GRACE;
    }

    /// Reverts unless the proof shows an identity in rootOpen and in rootNow
    /// (a known root), and that nullifier = H(scope, its secret).
    function verifyVote(uint256 rootOpen, uint256 rootNow, uint256 nullifier, uint256 scope, uint256 message, bytes calldata proof)
        external view {
        require(knownRoot(rootNow), "the citizens' tree changed: prove again");
        verifier.verifyProof(proof, [rootOpen, rootNow, nullifier, scope, message]);
    }
}
