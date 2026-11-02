// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

struct MetadataEntry8004 {
    string metadataKey;
    bytes metadataValue;
}

interface IIdentityRegistry8004 {
    function register(string calldata agentURI, MetadataEntry8004[] calldata metadata) external returns (uint256 agentId);
    function transferFrom(address from, address to, uint256 tokenId) external;
}

/// Registers an ERC-8004 agent for a smart account that can't take an
/// ERC-721 by safe transfer (ternchain's accounts have no onERC721Received,
/// and the Identity Registry mints with _safeMint): it registers, takes the
/// agent id itself, and hands it to the caller with a plain transfer. The
/// metadata (tern.name) is set while it holds the id.
contract ERC8004Registrar {
    IIdentityRegistry8004 public immutable identity;

    event RegisteredFor(uint256 indexed agentId, address indexed owner);

    constructor(IIdentityRegistry8004 identity_) {
        identity = identity_;
    }

    function register(string calldata agentURI, MetadataEntry8004[] calldata metadata) external returns (uint256 agentId) {
        agentId = identity.register(agentURI, metadata);
        identity.transferFrom(address(this), msg.sender, agentId);
        emit RegisteredFor(agentId, msg.sender);
    }

    function onERC721Received(address, address, uint256, bytes calldata) external pure returns (bytes4) {
        return this.onERC721Received.selector;
    }
}
