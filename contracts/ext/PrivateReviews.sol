// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

interface IReviewBank {
    function locks(uint256 id) external view returns (bytes32 payer, bytes32 from, bytes32 payee, uint256 total, uint256 paid,
        uint256 refunded, uint64 refundAfter, address condition, bytes memory demand, address token);
}

interface IReviewRegistry {
    function canAct(bytes32 id, address who) external view returns (bool ok, bytes32 owner, bool isAgent);
    function ownerOf(bytes32 id) external view returns (bytes32);
}

/// Private reviews: one leaf per paid Bank lock, posted by the payer's side,
///
///     leaf = H(H(s, lock), score + 100)      (Poseidon2; s: the payee's reputation secret)
///
/// so the chain shows that a lock was reviewed, not the score, and not which
/// later reputation proof counts it. The payee gives the payer H(s, lock) for
/// that one deal (sealed mail); later it proves in zero knowledge "N distinct
/// leaves of this tree are mine and their scores add up to at least X"
/// (internal/zk RepCircuit), and a verifier learns neither the deals nor the
/// counterparts. The contract doesn't hash: readers rebuild the tree from the
/// events (one tree for the whole network, depth 16), as for AnonGroups.
contract PrivateReviews {
    uint256 public constant FIELD = 21888242871839275222246405745257275088548364400416711140493589625536283799617;
    uint32 public constant MAX_LEAVES = 65536;

    IReviewBank public immutable bank;
    IReviewRegistry public immutable registry;
    uint32 public size;
    mapping(uint256 => bool) public reviewed; // lock => its leaf was posted

    event Review(uint256 indexed lock, uint32 index, uint256 leaf);

    constructor(IReviewBank bank_, IReviewRegistry registry_) {
        bank = bank_;
        registry = registry_;
    }

    /// by: the reviewer's name (the payer, or one of its agents); one leaf per lock.
    function review(uint256 lock, bytes32 by, uint256 leaf) external {
        (bytes32 payer,, bytes32 payee,, uint256 paid,,,,,) = bank.locks(lock);
        require(paid > 0, "the lock paid nothing: only a paid deal is reviewed");
        (bool ok, bytes32 owner,) = registry.canAct(by, msg.sender);
        require(ok && owner == payer, "only the payer's side reviews its deal");
        require(registry.ownerOf(payee) != payer, "no reviews of oneself");
        require(!reviewed[lock], "reviewed already");
        require(leaf != 0 && leaf < FIELD, "leaf");
        require(size < MAX_LEAVES, "the tree is full");
        reviewed[lock] = true;
        emit Review(lock, size, leaf);
        size++;
    }
}
