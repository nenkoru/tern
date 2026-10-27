// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";

/// Things worth money that live off chain: model weights, datasets, adapters,
/// reports. The chain keeps a hash, a size, where to fetch it, a licence, a
/// price and a commitment to the decryption key. The file itself is stored
/// anywhere (IPFS, Arweave, S3), usually encrypted; buying one is a sale in
/// the Bank, where the key is exchanged for payment.
contract Artifacts {
    Registry public immutable registry;

    struct Artifact {
        bytes32 owner;       // owner or agent id
        bytes32 contentHash; // sha-256 of the (encrypted) file
        uint64 size;
        string uri;
        string license;
        uint256 price;       // 0 = not for sale
        bytes32 keyCommit;   // keccak256 of the decryption key (0 = not encrypted)
        bool active;
    }

    Artifact[] public all;

    event Registered(uint256 indexed id, bytes32 indexed owner, bytes32 contentHash, uint64 size, string uri,
        string license, uint256 price, bytes32 keyCommit);
    event PriceSet(uint256 indexed id, uint256 price);
    event Retired(uint256 indexed id);

    constructor(Registry r) {
        registry = r;
    }

    function register(bytes32 owner, bytes32 contentHash, uint64 size, string calldata uri, string calldata license,
        uint256 price, bytes32 keyCommit) external returns (uint256 id) {
        (bool ok,,) = registry.canAct(owner, msg.sender);
        require(ok, "not yours");
        require(bytes(uri).length <= 512 && bytes(license).length <= 256, "size");
        id = all.length;
        all.push(Artifact(owner, contentHash, size, uri, license, price, keyCommit, true));
        emit Registered(id, owner, contentHash, size, uri, license, price, keyCommit);
    }

    function setPrice(uint256 id, uint256 price) external {
        (bool ok,,) = registry.canAct(all[id].owner, msg.sender);
        require(ok, "not yours");
        all[id].price = price;
        emit PriceSet(id, price);
    }

    function retire(uint256 id) external {
        (bool ok,,) = registry.canAct(all[id].owner, msg.sender);
        require(ok, "not yours");
        all[id].active = false;
        emit Retired(id);
    }

    function get(uint256 id) external view returns (bytes32 owner, uint256 price, bool active, bytes32 contentHash, bytes32 keyCommit) {
        Artifact storage a = all[id];
        return (a.owner, a.price, a.active, a.contentHash, a.keyCommit);
    }

    function count() external view returns (uint256) {
        return all.length;
    }
}
