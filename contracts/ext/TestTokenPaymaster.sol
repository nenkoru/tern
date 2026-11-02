// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import {IEntryPoint, IPaymaster, PackedUserOperation} from "@openzeppelin/contracts/interfaces/draft-IERC4337.sol";

interface IPermitToken {
    function permit(address owner, address spender, uint256 value, uint256 deadline, bytes calldata signature) external;
    function transferFrom(address from, address to, uint256 value) external returns (bool);
    function transfer(address to, uint256 value) external returns (bool);
}

/// A devnet's look-alike of Circle Paymaster v0.8 (Base Sepolia
/// 0x3BA9A96eE3eFf3A69E2B18886AcF52027EFF8966): the user pays its own gas in
/// USDC. paymasterAndData = paymaster | verificationGas(16) | postOpGas(16) |
/// mode(1) | token(20) | permitAmount(32) | permit signature (EIP-2612 by the
/// account, deadline max). It takes a prefund in the token at a fixed price
/// (3000 per ETH, as Circle's test oracle) and gives back what wasn't used.
contract TestTokenPaymaster is IPaymaster {
    IEntryPoint public immutable entryPoint;
    IPermitToken public immutable token;
    uint256 public constant PRICE = 3000e6; // token units per 1e18 wei (USDC: 6 decimals)

    constructor(IEntryPoint ep, IPermitToken token_) {
        entryPoint = ep;
        token = token_;
    }

    function deposit() external payable {
        entryPoint.depositTo{value: msg.value}(address(this));
    }

    function validatePaymasterUserOp(PackedUserOperation calldata op, bytes32, uint256 maxCost)
        external returns (bytes memory context, uint256 validationData) {
        require(msg.sender == address(entryPoint), "entry point only");
        bytes calldata pd = op.paymasterAndData;
        require(pd.length <= 53 || pd.length > 105, "MalformedPaymasterData");
        uint256 postGas = uint128(bytes16(pd[36:52]));
        if (pd.length > 105) {
            require(address(bytes20(pd[53:73])) == address(token), "token");
            try token.permit(op.sender, address(this), uint256(bytes32(pd[73:105])), type(uint256).max, pd[105:]) {} catch {}
        }
        uint256 maxFee = uint128(uint256(op.gasFees));
        uint256 prefund = ((postGas * maxFee + maxCost) * PRICE) / 1e18 + 1;
        require(token.transferFrom(op.sender, address(this), prefund), "AA33 token prefund");
        return (abi.encode(op.sender, prefund, postGas), 0);
    }

    function postOp(PostOpMode, bytes calldata context, uint256 actualGasCost, uint256 actualUserOpFeePerGas) external {
        require(msg.sender == address(entryPoint), "entry point only");
        (address sender, uint256 prefund, uint256 postGas) = abi.decode(context, (address, uint256, uint256));
        uint256 cost = ((actualGasCost + postGas * actualUserOpFeePerGas) * PRICE) / 1e18;
        if (cost < prefund) token.transfer(sender, prefund - cost);
    }
}
