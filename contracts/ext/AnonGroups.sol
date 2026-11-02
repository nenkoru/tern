// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// Anonymous group membership: per group, a tree of identity commitments
/// with no names (Semaphore v4 identities, one fresh identity per group).
/// Members prove "one of these leaves is mine" with the vote circuit's proof,
/// which readers check off chain; the chain keeps the leaves (in events), the
/// admin's roots and the bans. It doesn't hash: clients rebuild the tree from
/// the events and refuse every root its leaves don't give (an admin who posts
/// a wrong root breaks its group for everyone, visibly), so a change costs a
/// write, not sixteen Poseidon2 calls.
///
/// A group's id is random (gid): its name lives in the invitations, not here.
/// Who is in it, the chain doesn't know; how many, and when leaves came, it does.
contract AnonGroups {
    uint256 public constant FIELD = 21888242871839275222246405745257275088548364400416711140493589625536283799617; // BN254 scalar field
    uint32 public constant MAX_LEAVES = 65536; // depth 16, as the vote circuit's tree
    uint8 public constant PSEUDONYMOUS = 0; // one pseudonym per member and group: bans by pseudonym
    uint8 public constant UNLINKABLE = 1; // a fresh pseudonym per post, rate-limited by slots: no per-pseudonym bans

    struct Group {
        address admin;
        uint32 size; // leaves ever added (removed ones are zero leaves)
        uint8 mode;
        uint256 root;
    }

    mapping(bytes32 => Group) public groups;
    /// A root => the time it became the group's root (0: never); readers keep the rest.
    mapping(bytes32 => mapping(uint256 => uint64)) public rootSince;
    /// A pseudonym (the member's nullifier for the group's scope) => when it was banned (0: not banned).
    mapping(bytes32 => mapping(uint256 => uint64)) public bannedAt;

    event Created(bytes32 indexed gid, address admin, uint8 mode);
    event Leaves(bytes32 indexed gid, uint32 start, uint256[] leaves, uint256 root);
    event Removed(bytes32 indexed gid, uint32[] indexes, uint256 root);
    event Banned(bytes32 indexed gid, uint256 nullifier, bytes32 reason);
    event Unbanned(bytes32 indexed gid, uint256 nullifier);
    event AdminSet(bytes32 indexed gid, address admin);

    modifier onlyAdmin(bytes32 gid) {
        require(groups[gid].admin == msg.sender, "not the group's admin");
        _;
    }

    function create(bytes32 gid, uint8 mode) external {
        require(gid != bytes32(0), "gid");
        require(groups[gid].admin == address(0), "group exists");
        require(mode <= UNLINKABLE, "mode");
        groups[gid] = Group(msg.sender, 0, mode, 0);
        emit Created(gid, msg.sender, mode);
    }

    /// Adds leaves (a batch: single joins blur) and sets the root they give.
    function add(bytes32 gid, uint256[] calldata leaves, uint256 root) external onlyAdmin(gid) {
        Group storage g = groups[gid];
        require(leaves.length > 0 && g.size + leaves.length <= MAX_LEAVES, "leaves");
        for (uint256 i = 0; i < leaves.length; i++) {
            require(leaves[i] != 0 && leaves[i] < FIELD, "leaf");
        }
        uint32 start = g.size;
        g.size += uint32(leaves.length);
        emit Leaves(gid, start, leaves, root);
        _setRoot(gid, root);
    }

    /// Zeroes leaves (a member left or was removed) and sets the new root.
    function remove(bytes32 gid, uint32[] calldata indexes, uint256 root) external onlyAdmin(gid) {
        require(indexes.length > 0, "indexes");
        for (uint256 i = 0; i < indexes.length; i++) {
            require(indexes[i] < groups[gid].size, "index");
        }
        emit Removed(gid, indexes, root);
        _setRoot(gid, root);
    }

    function ban(bytes32 gid, uint256 nullifier, bytes32 reason) external onlyAdmin(gid) {
        require(groups[gid].mode == PSEUDONYMOUS, "unlinkable groups have no pseudonyms to ban");
        require(bannedAt[gid][nullifier] == 0, "banned already");
        bannedAt[gid][nullifier] = uint64(block.timestamp);
        emit Banned(gid, nullifier, reason);
    }

    function unban(bytes32 gid, uint256 nullifier) external onlyAdmin(gid) {
        require(bannedAt[gid][nullifier] != 0, "not banned");
        bannedAt[gid][nullifier] = 0;
        emit Unbanned(gid, nullifier);
    }

    function setAdmin(bytes32 gid, address admin) external onlyAdmin(gid) {
        require(admin != address(0), "admin");
        groups[gid].admin = admin;
        emit AdminSet(gid, admin);
    }

    function _setRoot(bytes32 gid, uint256 root) internal {
        require(root < FIELD, "root");
        groups[gid].root = root;
        if (rootSince[gid][root] == 0) rootSince[gid][root] = uint64(block.timestamp);
    }
}
