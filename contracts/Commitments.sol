// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";

/// Commit now, reveal later. Sealed bids, fair games, secret votes, and
/// markers: a commitment under a topic nobody else can compute, such as a
/// secret both sides of a pair derive from their keys (mutual likes that
/// reveal nothing unless both sides like). `by` may be zero for anonymous
/// commitments (the paying address is still visible; use a fresh one).
contract Commitments {
    Registry public immutable registry;

    struct Commitment {
        bytes32 by;
        bytes32 topic;
        bytes32 hash;
        uint64 revealBy; // 0 = never revealed (a marker)
        bool revealed;
        uint64 at;         // when it was made
        uint64 revealedAt;
        bytes32 opened;    // sha256(preimage), set at reveal: unknown before it (for randomness)
    }

    Commitment[] public all;
    mapping(bytes32 => uint256[]) internal byTopic;

    event Committed(uint256 indexed id, bytes32 indexed topic, bytes32 indexed by, bytes32 hash, uint64 revealBy);
    event Revealed(uint256 indexed id, bytes32 indexed topic, bytes preimage);

    constructor(Registry r) {
        registry = r;
    }

    function commit(bytes32 by, bytes32 topic, bytes32 hash, uint64 revealBy) external returns (uint256 id) {
        if (by != bytes32(0)) {
            (bool ok,,) = registry.canAct(by, msg.sender);
            require(ok, "not yours");
        }
        require(revealBy == 0 || revealBy > block.timestamp, "reveal time");
        id = all.length;
        all.push(Commitment(by, topic, hash, revealBy, false, uint64(block.timestamp), 0, bytes32(0)));
        byTopic[topic].push(id);
        emit Committed(id, topic, by, hash, revealBy);
    }

    function reveal(uint256 id, bytes calldata preimage) external {
        Commitment storage c = all[id];
        require(!c.revealed && c.revealBy != 0, "nothing to reveal");
        require(block.timestamp <= c.revealBy, "too late");
        require(keccak256(preimage) == c.hash, "wrong preimage");
        c.revealed = true;
        c.revealedAt = uint64(block.timestamp);
        c.opened = sha256(preimage);
        emit Revealed(id, c.topic, preimage);
    }

    function idsOf(bytes32 topic) external view returns (uint256[] memory) {
        return byTopic[topic];
    }

    function count() external view returns (uint256) {
        return all.length;
    }
}
