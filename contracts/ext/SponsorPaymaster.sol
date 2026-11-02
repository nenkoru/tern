// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import {IEntryPoint, IPaymaster, PackedUserOperation} from "@openzeppelin/contracts/interfaces/draft-IERC4337.sol";

interface IRegistryActs {
    function canAct(bytes32 id, address who) external view returns (bool ok, bytes32 owner, bool isAgent);
}

/// Pays the gas of ternchain's smart accounts on a host chain (Base Sepolia),
/// so a tester needs no ETH and no faucet that wants an account: the network's
/// operator funds this paymaster's EntryPoint deposit with free Sepolia ETH.
///
/// Sponsored: an operation from a name's account (the Registry says the
/// sender acts for the name in paymasterData), or the operation that deploys
/// an account with the network's AccountFactory (a newcomer's first step).
/// Each name (or new account) gets at most `perName` wei of gas a day, and
/// all of them together at most `perDay`: one tester can't drain it, nor can
/// many. paymasterAndData = paymaster | verificationGas(16) | postOpGas(16) | name id(32).
contract SponsorPaymaster is IPaymaster {
    IEntryPoint public immutable entryPoint;
    IRegistryActs public immutable registry;
    address public immutable factory;
    address public operator; // sets the caps and withdraws; the deposit is anyone's to add to

    uint256 public perName; // wei of gas per name (or new account) per day
    uint256 public perDay; // wei of gas per day for everyone
    mapping(bytes32 => mapping(uint256 => uint256)) public spent; // name id (or account) => day => wei
    mapping(uint256 => uint256) public spentDay; // day => wei

    event Sponsored(bytes32 indexed name, address indexed account, uint256 cost);
    event Caps(uint256 perName, uint256 perDay);
    event OperatorSet(address operator);

    constructor(IEntryPoint ep, IRegistryActs reg, address accountFactory, address op, uint256 perName_, uint256 perDay_) {
        entryPoint = ep;
        registry = reg;
        factory = accountFactory;
        operator = op;
        perName = perName_;
        perDay = perDay_;
        emit Caps(perName_, perDay_);
    }

    modifier onlyOperator() {
        require(msg.sender == operator, "the operator only");
        _;
    }

    function setCaps(uint256 perName_, uint256 perDay_) external onlyOperator {
        perName = perName_;
        perDay = perDay_;
        emit Caps(perName_, perDay_);
    }

    function setOperator(address op) external onlyOperator {
        operator = op;
        emit OperatorSet(op);
    }

    /// Anyone may add to the sponsored gas.
    function deposit() external payable {
        entryPoint.depositTo{value: msg.value}(address(this));
    }

    function withdraw(address payable to, uint256 amount) external onlyOperator {
        entryPoint.withdrawTo(to, amount);
    }

    /// A stake, for bundlers that want paymasters staked (ERC-7562).
    function addStake(uint32 unstakeDelaySec) external payable onlyOperator {
        entryPoint.addStake{value: msg.value}(unstakeDelaySec);
    }

    function unlockStake() external onlyOperator {
        entryPoint.unlockStake();
    }

    function withdrawStake(address payable to) external onlyOperator {
        entryPoint.withdrawStake(to);
    }

    function balance() external view returns (uint256) {
        return entryPoint.balanceOf(address(this));
    }

    /// What `name` (or a new account) may still spend today.
    function left(bytes32 name) external view returns (uint256) {
        uint256 day = block.timestamp / 1 days;
        uint256 a = perName > spent[name][day] ? perName - spent[name][day] : 0;
        uint256 b = perDay > spentDay[day] ? perDay - spentDay[day] : 0;
        return a < b ? a : b;
    }

    function validatePaymasterUserOp(PackedUserOperation calldata op, bytes32, uint256 maxCost)
        external returns (bytes memory context, uint256 validationData) {
        require(msg.sender == address(entryPoint), "entry point only");
        bytes32 key;
        if (op.paymasterAndData.length >= 84) {
            bytes32 name = bytes32(op.paymasterAndData[52:84]);
            (bool ok, , ) = registry.canAct(name, op.sender);
            if (ok) key = name;
        }
        if (key == bytes32(0)) { // not a name's account yet: only its first operation, deploying it with our factory
            require(op.initCode.length >= 20 && address(bytes20(op.initCode[:20])) == factory, "not a ternchain account");
            key = bytes32(uint256(uint160(op.sender)));
        }
        uint256 day = block.timestamp / 1 days;
        require(spent[key][day] + maxCost <= perName, "over this name's sponsored gas for today");
        require(spentDay[day] + maxCost <= perDay, "over today's sponsored gas for the network");
        spent[key][day] += maxCost;
        spentDay[day] += maxCost;
        return (abi.encode(key, op.sender, day, maxCost), 0);
    }

    function postOp(PostOpMode, bytes calldata context, uint256 actualGasCost, uint256) external {
        require(msg.sender == address(entryPoint), "entry point only");
        (bytes32 key, address account, uint256 day, uint256 maxCost) = abi.decode(context, (bytes32, address, uint256, uint256));
        uint256 back = maxCost > actualGasCost ? maxCost - actualGasCost : 0;
        spent[key][day] -= back;
        spentDay[day] -= back;
        emit Sponsored(key, account, maxCost - back);
    }
}
