// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import {Account} from "@openzeppelin/contracts/account/Account.sol";
import {ERC7821} from "@openzeppelin/contracts/account/extensions/draft-ERC7821.sol";
import {SignerWebAuthn} from "@openzeppelin/contracts/utils/cryptography/signers/SignerWebAuthn.sol";
import {SignerP256} from "@openzeppelin/contracts/utils/cryptography/signers/SignerP256.sol";
import {SignerECDSA} from "@openzeppelin/contracts/utils/cryptography/signers/SignerECDSA.sol";
import {ECDSA} from "@openzeppelin/contracts/utils/cryptography/ECDSA.sol";
import {Create2} from "@openzeppelin/contracts/utils/Create2.sol";
import {IEntryPoint, IPaymaster, PackedUserOperation} from "@openzeppelin/contracts/interfaces/draft-IERC4337.sol";

/// Smart accounts (ERC-4337, OpenZeppelin Account, ERC-7821 batch execution).
///
/// An owner's account is guarded by a passkey (WebAuthn, P-256): the key never
/// leaves the phone or laptop that made it. A separate mail key may sign
/// messages (ERC-1271), never transactions. An agent's account is guarded by
/// the agent's session key; the agent holds no ETH: its gas is paid by the
/// paymaster from its owner's deposit, within a budget per epoch the owner sets.

contract OwnerAccount is Account, SignerWebAuthn, ERC7821 {
    IEntryPoint private immutable _ep;
    address public mailKey; // signs mail only (isValidSignature with 65 bytes); set by the passkey

    event MailKeySet(address key);
    event PasskeyRotated(bytes32 qx, bytes32 qy);

    constructor(IEntryPoint ep, bytes32 qx, bytes32 qy, address mail) SignerP256(qx, qy) {
        _ep = ep;
        mailKey = mail;
    }

    function entryPoint() public view override returns (IEntryPoint) {
        return _ep;
    }

    function _erc7821AuthorizedExecutor(address caller, bytes32 mode, bytes calldata data) internal view override returns (bool) {
        return caller == address(_ep) || super._erc7821AuthorizedExecutor(caller, mode, data);
    }

    /// Only through the account itself (a passkey-signed operation).
    function setMailKey(address key) external {
        require(msg.sender == address(this), "the account only");
        mailKey = key;
        emit MailKeySet(key);
    }

    /// A new passkey (a new phone): only through the account itself.
    function rotatePasskey(bytes32 qx, bytes32 qy) external {
        require(msg.sender == address(this), "the account only");
        _setSigner(qx, qy);
        emit PasskeyRotated(qx, qy);
    }

    /// ERC-1271: a passkey assertion, or (65 bytes) the mail key's signature.
    function isValidSignature(bytes32 hash, bytes calldata signature) external view returns (bytes4) {
        if (signature.length == 65) {
            (address r, ECDSA.RecoverError err, ) = ECDSA.tryRecover(hash, signature);
            return err == ECDSA.RecoverError.NoError && r == mailKey && r != address(0) ? bytes4(0x1626ba7e) : bytes4(0xffffffff);
        }
        return _rawSignatureValidation(hash, signature) ? bytes4(0x1626ba7e) : bytes4(0xffffffff);
    }
}

contract AgentAccount is Account, SignerECDSA, ERC7821 {
    IEntryPoint private immutable _ep;

    constructor(IEntryPoint ep, address sessionKey) SignerECDSA(sessionKey) {
        _ep = ep;
    }

    function entryPoint() public view override returns (IEntryPoint) {
        return _ep;
    }

    function _erc7821AuthorizedExecutor(address caller, bytes32 mode, bytes calldata data) internal view override returns (bool) {
        return caller == address(_ep) || super._erc7821AuthorizedExecutor(caller, mode, data);
    }

    function isValidSignature(bytes32 hash, bytes calldata signature) external view returns (bytes4) {
        return _rawSignatureValidation(hash, signature) ? bytes4(0x1626ba7e) : bytes4(0xffffffff);
    }
}

/// Deterministic addresses: an account's address is known before it exists,
/// so a name can be registered to it and money sent to it; the first
/// operation deploys it (ERC-4337 initCode).
contract AccountFactory {
    IEntryPoint public immutable entryPoint;

    constructor(IEntryPoint ep) {
        entryPoint = ep;
    }

    function ownerAccount(bytes32 qx, bytes32 qy, address mail, bytes32 salt) external returns (address a) {
        a = ownerAddress(qx, qy, mail, salt);
        if (a.code.length == 0) new OwnerAccount{salt: salt}(entryPoint, qx, qy, mail);
    }

    function agentAccount(address sessionKey, bytes32 salt) external returns (address a) {
        a = agentAddress(sessionKey, salt);
        if (a.code.length == 0) new AgentAccount{salt: salt}(entryPoint, sessionKey);
    }

    function ownerAddress(bytes32 qx, bytes32 qy, address mail, bytes32 salt) public view returns (address) {
        return Create2.computeAddress(salt, keccak256(abi.encodePacked(type(OwnerAccount).creationCode, abi.encode(entryPoint, qx, qy, mail))));
    }

    function agentAddress(address sessionKey, bytes32 salt) public view returns (address) {
        return Create2.computeAddress(salt, keccak256(abi.encodePacked(type(AgentAccount).creationCode, abi.encode(entryPoint, sessionKey))));
    }
}

/// Pays gas for names' accounts from their owners' deposits. An agent's gas
/// counts against a budget per epoch that its owner sets, the same way the
/// Bank counts its payments: an agent can't burn its owner's money on gas.
/// paymasterAndData = paymaster | verificationGas(16) | postOpGas(16) | name id(32).
contract TernPaymaster is IPaymaster {
    IEntryPoint public immutable entryPoint;
    Registry public immutable registry;

    mapping(bytes32 => uint256) public deposit; // owner id => wei for gas
    mapping(bytes32 => uint256) public gasBudget; // agent id => wei per epoch
    mapping(bytes32 => mapping(uint64 => uint256)) public gasSpent;

    event Funded(bytes32 indexed owner, uint256 amount);
    event BudgetSet(bytes32 indexed agent, uint256 perEpoch);
    event Charged(bytes32 indexed name, bytes32 indexed owner, uint256 cost);

    constructor(IEntryPoint ep, Registry r) {
        entryPoint = ep;
        registry = r;
    }

    /// Anyone may fund an owner's gas.
    function fund(bytes32 owner) external payable {
        require(registry.ownerOf(owner) == owner, "no owner");
        deposit[owner] += msg.value;
        entryPoint.depositTo{value: msg.value}(address(this));
        emit Funded(owner, msg.value);
    }

    function withdraw(bytes32 owner, address payable to, uint256 amount) external {
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && registry.ownerOf(owner) == owner, "owner only");
        require(deposit[owner] >= amount, "deposit");
        deposit[owner] -= amount;
        entryPoint.withdrawTo(to, amount);
    }

    function setGasBudget(bytes32 agent, uint256 perEpoch) external {
        bytes32 owner = registry.ownerOf(agent);
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && owner != agent, "the agent's owner only");
        gasBudget[agent] = perEpoch;
        emit BudgetSet(agent, perEpoch);
    }

    function validatePaymasterUserOp(PackedUserOperation calldata op, bytes32, uint256 maxCost)
        external returns (bytes memory context, uint256 validationData) {
        require(msg.sender == address(entryPoint), "entry point only");
        require(op.paymasterAndData.length >= 84, "name missing");
        bytes32 name = bytes32(op.paymasterAndData[52:84]);
        (bool ok, bytes32 owner, bool isAgent) = registry.canAct(name, op.sender);
        require(ok, "not this name's account");
        require(deposit[owner] >= maxCost, "the owner's gas deposit is too low");
        deposit[owner] -= maxCost;
        uint64 e;
        if (isAgent) {
            e = registry.epoch();
            require(gasSpent[name][e] + maxCost <= gasBudget[name], "over the agent's gas budget");
            gasSpent[name][e] += maxCost;
        }
        return (abi.encode(name, owner, isAgent, maxCost, e), 0);
    }

    function postOp(PostOpMode, bytes calldata context, uint256 actualGasCost, uint256) external {
        require(msg.sender == address(entryPoint), "entry point only");
        (bytes32 name, bytes32 owner, bool isAgent, uint256 maxCost, uint64 e) = abi.decode(context, (bytes32, bytes32, bool, uint256, uint64));
        uint256 back = maxCost > actualGasCost ? maxCost - actualGasCost : 0;
        deposit[owner] += back;
        if (isAgent) gasSpent[name][e] -= back;
        emit Charged(name, owner, maxCost - back);
    }
}
