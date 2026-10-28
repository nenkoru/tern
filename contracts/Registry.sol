// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// Names, owner accounts, recovery with a delay and a veto, agent grants,
/// published keys and cards. The chain is the log: every change is an event,
/// in one global order. Content is never verified here; clients do that.
contract Registry {
    uint64 public immutable epochLength;   // seconds per key epoch
    uint64 public immutable recoveryDelay; // seconds before a recovery takes effect

    struct Owner {
        address controller;      // the owner's key (a passkey account later)
        address guardian;        // may start a recovery
        uint32 version;          // bumped by recovery or a compromised rotation
        address pendingController;
        uint64 pendingEffective;
        string name;
    }

    struct Grant {
        bytes32 owner;           // owner id
        address agent;           // the agent's signing address
        uint64 expires;
        uint32 generation;
        uint32 ownerVersion;     // owner version the grant was issued under
        bool revoked;
        uint128 spendPerEpoch;   // limit for the Bank, in wei
        string name;             // "mika.ruslan"
    }

    mapping(bytes32 => Owner) public owners; // keccak256(name)
    mapping(bytes32 => Grant) public grants; // keccak256(full agent name)
    mapping(bytes32 => bytes32) public keysCommit;
    mapping(bytes32 => uint64) public registeredAt; // owner id => when the name was taken (its age counts for citizenship)

    event OwnerRegistered(bytes32 indexed id, string name, address controller, address guardian);
    event ControllerChanged(bytes32 indexed id, address controller, uint32 version, bool recovery);
    event GuardianChanged(bytes32 indexed id, address guardian);
    event RecoveryStarted(bytes32 indexed id, address newController, uint64 effective);
    event RecoveryCancelled(bytes32 indexed id);
    event AgentGranted(bytes32 indexed id, bytes32 indexed owner, string name, address agent, uint64 expires,
        uint32 generation, uint128 spendPerEpoch, string scopes);
    event AgentRevoked(bytes32 indexed id, uint32 generation, bool compromised);
    event KeysPublished(bytes32 indexed id, uint64 firstEpoch, bytes viewKey, bytes epochKeys);
    event CardSet(bytes32 indexed id, string card);

    constructor(uint64 _epochLength, uint64 _recoveryDelay) {
        require(_epochLength > 0, "epoch");
        epochLength = _epochLength;
        recoveryDelay = _recoveryDelay;
    }

    function epoch() public view returns (uint64) {
        return uint64(block.timestamp) / epochLength;
    }

    function idOf(string memory name) public pure returns (bytes32) {
        return keccak256(bytes(name));
    }

    // label = [a-z0-9-]{1,32}, not starting or ending with "-"
    function validLabel(string memory s) public pure returns (bool) {
        bytes memory b = bytes(s);
        if (b.length == 0 || b.length > 32 || b[0] == "-" || b[b.length - 1] == "-") return false;
        for (uint256 i = 0; i < b.length; i++) {
            bytes1 c = b[i];
            if (!((c >= "a" && c <= "z") || (c >= "0" && c <= "9") || c == "-")) return false;
        }
        return true;
    }

    // ---- owners

    function register(string calldata name, address guardian) external returns (bytes32 id) {
        require(validLabel(name), "bad name");
        id = idOf(name);
        require(owners[id].controller == address(0), "taken");
        require(guardian != address(0) && guardian != msg.sender, "guardian must be another key");
        Owner storage o = owners[id];
        o.controller = msg.sender;
        o.guardian = guardian;
        o.name = name;
        registeredAt[id] = uint64(block.timestamp);
        emit OwnerRegistered(id, name, msg.sender, guardian);
    }

    /// Planned key change. compromised=true voids grants issued under the old key.
    function setController(bytes32 id, address next, bool compromised) external {
        Owner storage o = owners[id];
        require(msg.sender == o.controller, "not controller");
        require(next != address(0) && next != o.guardian, "bad controller");
        o.controller = next;
        if (compromised) o.version++;
        emit ControllerChanged(id, next, o.version, false);
        // A live owner rotating its key also cancels any recovery in progress:
        // that recovery was started against the old key, possibly by a thief.
        if (o.pendingController != address(0)) {
            o.pendingController = address(0);
            o.pendingEffective = 0;
            emit RecoveryCancelled(id);
        }
    }

    function setGuardian(bytes32 id, address guardian) external {
        Owner storage o = owners[id];
        require(msg.sender == o.controller, "not controller");
        require(guardian != address(0) && guardian != o.controller, "bad guardian");
        o.guardian = guardian;
        if (o.pendingController != address(0)) {
            o.pendingController = address(0);
            o.pendingEffective = 0;
            emit RecoveryCancelled(id);
        }
        emit GuardianChanged(id, guardian);
    }

    function startRecovery(bytes32 id, address next) external {
        Owner storage o = owners[id];
        require(o.controller != address(0), "no owner");
        require(msg.sender == o.guardian, "not guardian");
        require(next != address(0), "bad controller");
        o.pendingController = next;
        o.pendingEffective = uint64(block.timestamp) + recoveryDelay;
        emit RecoveryStarted(id, next, o.pendingEffective);
    }

    /// The current key vetoes a recovery it did not want.
    function cancelRecovery(bytes32 id) external {
        Owner storage o = owners[id];
        require(msg.sender == o.controller, "not controller");
        require(o.pendingController != address(0), "nothing pending");
        o.pendingController = address(0);
        o.pendingEffective = 0;
        emit RecoveryCancelled(id);
    }

    function finishRecovery(bytes32 id) external {
        Owner storage o = owners[id];
        require(o.pendingController != address(0), "nothing pending");
        require(block.timestamp >= o.pendingEffective, "too early");
        o.controller = o.pendingController;
        o.pendingController = address(0);
        o.pendingEffective = 0;
        o.version++; // the old key may be in someone else's hands
        emit ControllerChanged(id, o.controller, o.version, true);
    }

    // ---- agents

    function grant(bytes32 owner, string calldata label, address agent, uint64 expires, uint128 spendPerEpoch,
        string calldata scopes) external returns (bytes32 id) {
        Owner storage o = owners[owner];
        require(msg.sender == o.controller, "not controller");
        require(validLabel(label), "bad label");
        require(agent != address(0), "bad agent");
        require(expires > block.timestamp && expires <= block.timestamp + 90 days, "expiry 1s..90d");
        string memory name = string.concat(label, ".", o.name);
        id = idOf(name);
        Grant storage g = grants[id];
        g.owner = owner;
        g.agent = agent;
        g.expires = expires;
        g.generation++;
        g.ownerVersion = o.version;
        g.revoked = false;
        g.spendPerEpoch = spendPerEpoch;
        g.name = name;
        emit AgentGranted(id, owner, name, agent, expires, g.generation, spendPerEpoch, scopes);
    }

    function revoke(bytes32 id, bool compromised) external {
        Grant storage g = grants[id];
        require(msg.sender == owners[g.owner].controller, "not controller");
        g.revoked = true;
        emit AgentRevoked(id, g.generation, compromised);
    }

    function grantValid(bytes32 id) public view returns (bool) {
        Grant storage g = grants[id];
        return g.agent != address(0) && !g.revoked && block.timestamp < g.expires
            && g.ownerVersion == owners[g.owner].version;
    }

    /// Who may act for id (an owner or an agent), and which owner stands behind it.
    function canAct(bytes32 id, address who) public view returns (bool ok, bytes32 owner, bool isAgent) {
        if (owners[id].controller != address(0)) {
            return (owners[id].controller == who, id, false);
        }
        Grant storage g = grants[id];
        if (g.agent == address(0)) return (false, bytes32(0), false);
        bool asAgent = grantValid(id) && g.agent == who;
        bool asOwner = owners[g.owner].controller == who;
        return (asAgent || asOwner, g.owner, asAgent && !asOwner);
    }

    function ownerOf(bytes32 id) public view returns (bytes32) {
        if (owners[id].controller != address(0)) return id;
        return grants[id].owner;
    }

    function spendLimit(bytes32 id) external view returns (uint128) {
        return grants[id].spendPerEpoch;
    }

    // ---- keys and cards

    /// viewKey: X25519 public key for tags. epochKeys: newline-separated age
    /// recipients, one per epoch from firstEpoch on.
    function publishKeys(bytes32 id, uint64 firstEpoch, bytes calldata viewKey, bytes calldata epochKeys) external {
        (bool ok,,) = canAct(id, msg.sender);
        require(ok, "not yours");
        require(viewKey.length == 32, "view key");
        keysCommit[id] = keccak256(abi.encode(firstEpoch, viewKey, epochKeys));
        emit KeysPublished(id, firstEpoch, viewKey, epochKeys);
    }

    function setCard(bytes32 id, string calldata card) external {
        (bool ok,,) = canAct(id, msg.sender);
        require(ok, "not yours");
        require(bytes(card).length <= 4096, "card too long");
        emit CardSet(id, card);
    }
}
