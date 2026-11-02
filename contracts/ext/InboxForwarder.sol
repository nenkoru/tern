// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

interface IInboxPost {
    function fee() external view returns (uint256);
    function post(bytes calldata batch, uint256 n) external payable;
}

/// Mail on chain for senders who hold no coin. The Inbox charges each
/// envelope a fee in the chain's coin; on a network whose users hold only
/// USDC (the public profile on Base Sepolia, where the fee is set to its
/// floor, 1 wei), this pays that fee from its own balance, which anybody may
/// top up (it was seeded at deployment). Senders pay only their gas. It has
/// no owner and no settings; the Inbox refunds what it overpays.
contract InboxForwarder {
    IInboxPost public immutable inbox;

    event Funded(address indexed from, uint256 amount);

    constructor(IInboxPost inbox_) payable {
        inbox = inbox_;
        if (msg.value > 0) emit Funded(msg.sender, msg.value);
    }

    receive() external payable {
        if (msg.sender != address(inbox)) emit Funded(msg.sender, msg.value);
    }

    /// Posts a batch (1..256 sealed envelopes) as Inbox.post would, the fee paid from here.
    function post(bytes calldata batch, uint256 n) external {
        uint256 v = inbox.fee() * n * 2;
        require(address(this).balance >= v, "the forwarder is empty: top it up (send it any coin)");
        inbox.post{value: v}(batch, n);
    }
}
