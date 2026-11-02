// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {EIP712} from "@openzeppelin/contracts/utils/cryptography/EIP712.sol";
import {SignatureChecker} from "@openzeppelin/contracts/utils/cryptography/SignatureChecker.sol";

/// Test money shaped like Circle's USDC (devnets and tests only: anyone
/// mints): 6 decimals and EIP-3009 transferWithAuthorization under the same
/// EIP-712 domain ("USDC", "2"), so x402 payments and USDC settlement are
/// tried on a devnet exactly as on Base Sepolia.
contract TestUSDC is ERC20, EIP712 {
    bytes32 public constant TRANSFER_WITH_AUTHORIZATION_TYPEHASH =
        keccak256("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)");

    bytes32 public constant PERMIT_TYPEHASH =
        keccak256("Permit(address owner,address spender,uint256 value,uint256 nonce,uint256 deadline)");

    mapping(address => mapping(bytes32 => bool)) public authorizationState;
    mapping(address => uint256) public nonces; // EIP-2612

    event AuthorizationUsed(address indexed authorizer, bytes32 indexed nonce);

    constructor() ERC20("USDC", "USDC") EIP712("USDC", "2") {}

    function decimals() public pure override returns (uint8) {
        return 6;
    }

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }

    function DOMAIN_SEPARATOR() external view returns (bytes32) {
        return _domainSeparatorV4();
    }

    /// EIP-2612, the bytes form USDC has: an ECDSA signature or a contract wallet's (ERC-1271).
    function permit(address owner, address spender, uint256 value, uint256 deadline, bytes memory signature) public {
        require(deadline >= block.timestamp, "permit is expired");
        bytes32 digest = _hashTypedDataV4(keccak256(abi.encode(PERMIT_TYPEHASH, owner, spender, value, nonces[owner]++, deadline)));
        require(SignatureChecker.isValidSignatureNow(owner, digest, signature), "invalid signature");
        _approve(owner, spender, value);
    }

    function permit(address owner, address spender, uint256 value, uint256 deadline, uint8 v, bytes32 r, bytes32 s) external {
        permit(owner, spender, value, deadline, abi.encodePacked(r, s, v));
    }

    function transferWithAuthorization(address from, address to, uint256 value, uint256 validAfter, uint256 validBefore,
        bytes32 nonce, uint8 v, bytes32 r, bytes32 s) external {
        _transferWithAuthorization(from, to, value, validAfter, validBefore, nonce, abi.encodePacked(r, s, v));
    }

    /// The bytes form: an ECDSA signature, or a contract wallet's (ERC-1271).
    function transferWithAuthorization(address from, address to, uint256 value, uint256 validAfter, uint256 validBefore,
        bytes32 nonce, bytes memory signature) external {
        _transferWithAuthorization(from, to, value, validAfter, validBefore, nonce, signature);
    }

    function _transferWithAuthorization(address from, address to, uint256 value, uint256 validAfter, uint256 validBefore,
        bytes32 nonce, bytes memory signature) internal {
        require(block.timestamp > validAfter, "authorization is not yet valid");
        require(block.timestamp < validBefore, "authorization is expired");
        require(!authorizationState[from][nonce], "authorization is used or canceled");
        bytes32 digest = _hashTypedDataV4(keccak256(abi.encode(TRANSFER_WITH_AUTHORIZATION_TYPEHASH, from, to, value, validAfter, validBefore, nonce)));
        require(SignatureChecker.isValidSignatureNow(from, digest, signature), "invalid signature");
        authorizationState[from][nonce] = true;
        emit AuthorizationUsed(from, nonce);
        _transfer(from, to, value);
    }
}
