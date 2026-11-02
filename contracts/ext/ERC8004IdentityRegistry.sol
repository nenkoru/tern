// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// ERC-8004 (Trustless Agents) Identity Registry for devnets and tests: the
/// same calls and events as the registries deployed on Base Sepolia
/// (0x8004A818…BD9e, getVersion 2.0.0), without their upgradeability. On
/// Base Sepolia ternchain uses the deployed ones (net.json "erc8004"). The
/// agent ids are ERC-721 tokens there; here only their owner is kept (enough
/// for registering, metadata and the reputation registry's checks).
contract ERC8004IdentityRegistry {
    struct MetadataEntry {
        string metadataKey;
        bytes metadataValue;
    }

    string public constant name = "AgentIdentity";
    string public constant symbol = "AGENT";
    uint256 private _next;
    mapping(uint256 => address) private _owner;
    mapping(uint256 => string) private _uri;
    mapping(uint256 => mapping(string => bytes)) private _meta;

    event Transfer(address indexed from, address indexed to, uint256 indexed tokenId);
    event Registered(uint256 indexed agentId, string agentURI, address indexed owner);
    event URIUpdated(uint256 indexed agentId, string newURI, address indexed updatedBy);
    event MetadataSet(uint256 indexed agentId, string indexed indexedMetadataKey, string metadataKey, bytes metadataValue);

    function getVersion() external pure returns (string memory) {
        return "2.0.0";
    }

    function register(string calldata agentURI, MetadataEntry[] calldata metadata) external returns (uint256 agentId) {
        agentId = _register(agentURI);
        for (uint256 i = 0; i < metadata.length; i++) _setMetadata(agentId, metadata[i].metadataKey, metadata[i].metadataValue);
    }

    function register(string calldata agentURI) external returns (uint256) {
        return _register(agentURI);
    }

    function register() external returns (uint256) {
        return _register("");
    }

    function _register(string memory agentURI) internal returns (uint256 agentId) {
        agentId = _next++;
        _owner[agentId] = msg.sender;
        emit Transfer(address(0), msg.sender, agentId);
        if (msg.sender.code.length > 0) { // _safeMint, as the deployed registry: a contract must take ERC-721s
            (bool ok, bytes memory r) = msg.sender.call(abi.encodeWithSignature("onERC721Received(address,address,uint256,bytes)", msg.sender, address(0), agentId, ""));
            require(ok && r.length == 32 && bytes4(r) == 0x150b7a02, "ERC721InvalidReceiver");
        }
        _uri[agentId] = agentURI;
        emit Registered(agentId, agentURI, msg.sender);
    }

    function transferFrom(address from, address to, uint256 agentId) external {
        require(from == _owner[agentId] && msg.sender == from && to != address(0), "Not authorized");
        _owner[agentId] = to;
        emit Transfer(from, to, agentId);
    }

    function ownerOf(uint256 agentId) public view returns (address o) {
        o = _owner[agentId];
        require(o != address(0), "no such agent");
    }

    function tokenURI(uint256 agentId) external view returns (string memory) {
        ownerOf(agentId);
        return _uri[agentId];
    }

    function setAgentURI(uint256 agentId, string calldata newURI) external {
        require(msg.sender == ownerOf(agentId), "Not authorized");
        _uri[agentId] = newURI;
        emit URIUpdated(agentId, newURI, msg.sender);
    }

    function setMetadata(uint256 agentId, string calldata metadataKey, bytes calldata metadataValue) external {
        require(msg.sender == ownerOf(agentId), "Not authorized");
        require(keccak256(bytes(metadataKey)) != keccak256("agentWallet"), "reserved key");
        _setMetadata(agentId, metadataKey, metadataValue);
    }

    function _setMetadata(uint256 agentId, string memory key, bytes memory value) internal {
        _meta[agentId][key] = value;
        emit MetadataSet(agentId, key, key, value);
    }

    function getMetadata(uint256 agentId, string calldata metadataKey) external view returns (bytes memory) {
        return _meta[agentId][metadataKey];
    }

    function getAgentWallet(uint256 agentId) external view returns (address) {
        return ownerOf(agentId);
    }

    /// The reputation registry's check: the agent's owner or an approved operator.
    function isAuthorizedOrOwner(address spender, uint256 agentId) external view returns (bool) {
        return spender == _owner[agentId];
    }
}
