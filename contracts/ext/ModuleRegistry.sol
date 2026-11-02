// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

interface IModuleNames {
    function canAct(bytes32 id, address who) external view returns (bool ok, bytes32 owner, bool isAgent);
}

/// A payment rail: a third party's way to move money for a payer under its
/// own rules (a shielded pool, a streaming rail, another settlement). The
/// core never routes through one: a user or group opts in, and their client
/// calls it. Its money moves are its own; ternchain's Bank and locks stay as
/// they are.
interface IPaymentRail {
    /// Pays `amount` of `token` (zero: the coin) as `data` says, for the caller; returns a receipt id.
    function pay(address token, uint256 amount, bytes calldata data) external payable returns (bytes32 receipt);
    /// What it is, for people: a name and a URI to its spec and audits.
    function railInfo() external view returns (string memory name, string memory uri);
}

/// A proof verifier: a zk (or any) proof checked on chain, for modules that
/// need one (a shielded pool, a private vote). Opt-in per module.
interface IProofVerifier {
    function verify(bytes calldata proof, uint256[] calldata publicInputs) external view returns (bool);
}

/// Modules anyone may publish, in their own namespace, with nobody to approve
/// them (a network without governance has nobody to ask): publisher name →
/// module name → versions, each a contract with its ABI's hash, the client
/// plugin that speaks to it, and a URI to its spec. Like SchemaNames for
/// types. Clients list them with their publisher and the claims about them,
/// warn for unaudited ones, and use one only when its user's owner opts in
/// (the owner's trust list). Publishing endorses nothing.
contract ModuleRegistry {
    struct Version {
        address module;
        bytes32 abiHash;
        string plugin; // the client plugin id ("" : the generic contract commands)
        string uri; // spec, source, audits
        uint64 at;
    }

    IModuleNames public immutable registry;
    mapping(bytes32 => mapping(bytes32 => Version[])) private _versions; // publisher id => keccak(name) => versions

    event Published(bytes32 indexed publisher, bytes32 indexed nameHash, string name, uint256 version, address module, bytes32 abiHash,
        string plugin, string uri);

    constructor(IModuleNames registry_) {
        registry = registry_;
    }

    /// A new version of `name` under the publisher's namespace (its owner, not an agent, publishes).
    function publish(bytes32 publisher, string calldata name, address module, bytes32 abiHash, string calldata plugin, string calldata uri)
        external returns (uint256 version) {
        (bool ok, , bool isAgent) = registry.canAct(publisher, msg.sender);
        require(ok && !isAgent, "the publisher's owner only");
        require(bytes(name).length > 0 && bytes(name).length <= 64, "a name of 1..64 bytes");
        require(module.code.length > 0, "not a contract");
        Version[] storage vs = _versions[publisher][keccak256(bytes(name))];
        vs.push(Version(module, abiHash, plugin, uri, uint64(block.timestamp)));
        version = vs.length; // 1-based
        emit Published(publisher, keccak256(bytes(name)), name, version, module, abiHash, plugin, uri);
    }

    function versions(bytes32 publisher, string calldata name) external view returns (uint256) {
        return _versions[publisher][keccak256(bytes(name))].length;
    }

    function get(bytes32 publisher, string calldata name, uint256 version) external view returns (Version memory) {
        return _versions[publisher][keccak256(bytes(name))][version - 1];
    }
}
