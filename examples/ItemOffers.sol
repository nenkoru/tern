// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC1155} from "@openzeppelin/contracts/token/ERC1155/IERC1155.sol";
import {ERC1155Holder} from "@openzeppelin/contracts/token/ERC1155/utils/ERC1155Holder.sol";

interface IRegistry {
    function canAct(bytes32 id, address who) external view returns (bool ok, bytes32 owner, bool isAgent);
    function ownerOf(bytes32 id) external view returns (bytes32);
}

interface IBank {
    function registry() external view returns (IRegistry);
    function isPayee(bytes32 id) external view returns (bool);
    function remaining(uint256 id) external view returns (uint256);
    function claim(uint256 id, bytes calldata fulfillment) external;
    function locks(uint256 id) external view returns (bytes32 payer, bytes32 from, bytes32 payee, uint256 total, uint256 paid,
        uint256 refunded, uint64 refundAfter, address condition, bytes memory demand, address token);
}

/// An offer book for one ERC-1155 collection (the offer book that once lived
/// inside the network's Items): the seller offers items here (held until sold
/// or cancelled), the buyer locks the price in the Bank with this contract as
/// the condition and the offer id as the demand, and take() hands over the
/// items and pays the seller in one transaction. The network's own route is
/// Bank.lockItem with SwapCondition; this is the same idea as a product
/// anyone can deploy and change. Unaudited.
contract ItemOffers is ERC1155Holder {
    IERC1155 public immutable items;
    IBank public immutable bank;

    struct Offer {
        address seller;
        bytes32 sellerName;
        uint256 item;
        uint256 amount;
        uint256 price;
        address token; // 0 = the network coin
        bytes32 buyer; // 0 = anyone
        bytes32 payTo; // who gets the money: the seller, or e.g. a split with co-authors
        uint256 lockId;
        uint8 state; // 1 open, 2 sold, 3 cancelled
    }

    Offer[] public offers;

    event Offered(uint256 indexed offer, uint256 indexed item, bytes32 indexed seller, uint256 amount, uint256 price, address token, bytes32 buyer);
    event Sold(uint256 indexed offer, uint256 lockId, address to);
    event Cancelled(uint256 indexed offer);

    constructor(IERC1155 i, IBank b) {
        items = i;
        bank = b;
    }

    /// The seller first approves this contract on the collection (setApprovalForAll).
    function offer(bytes32 sellerName, uint256 item, uint256 amount, uint256 price, address token, bytes32 buyer, bytes32 payTo)
        external returns (uint256 id) {
        (bool ok,,) = bank.registry().canAct(sellerName, msg.sender);
        require(ok, "not yours");
        require(amount > 0 && price > 0, "amount and price");
        if (payTo == bytes32(0)) payTo = sellerName;
        require(bank.isPayee(payTo), "pay-to must be a name or a split");
        items.safeTransferFrom(msg.sender, address(this), item, amount, "");
        id = offers.length;
        offers.push(Offer(msg.sender, sellerName, item, amount, price, token, buyer, payTo, 0, 1));
        emit Offered(id, item, sellerName, amount, price, token, buyer);
    }

    function cancel(uint256 id) external {
        Offer storage o = offers[id];
        (bool ok,,) = bank.registry().canAct(o.sellerName, msg.sender);
        require(ok && o.state == 1, "not yours or not open");
        o.state = 3;
        items.safeTransferFrom(address(this), o.seller, o.item, o.amount, "");
        emit Cancelled(id);
    }

    /// The buyer has locked the price (payee: payTo, condition: this contract,
    /// demand: abi.encode(offer id)). The items go to `to`, the seller is paid.
    function take(uint256 id, uint256 lockId, address to) external {
        Offer storage o = offers[id];
        require(o.state == 1, "not open");
        (, bytes32 from, bytes32 payee, uint256 total, , , , address cond, bytes memory demand, address token) = bank.locks(lockId);
        require(payee == o.payTo && cond == address(this) && token == o.token, "not a lock for this offer");
        require(keccak256(demand) == keccak256(abi.encode(id)), "lock is for another offer");
        require(total >= o.price && bank.remaining(lockId) >= o.price, "price not locked");
        IRegistry r = bank.registry();
        (bool ok,,) = r.canAct(from, msg.sender);
        require(ok, "the buyer takes");
        require(o.buyer == bytes32(0) || o.buyer == from || o.buyer == r.ownerOf(from), "reserved for someone else");
        o.state = 2;
        o.lockId = lockId;
        items.safeTransferFrom(address(this), to, o.item, o.amount, "");
        bank.claim(lockId, "");
        emit Sold(id, lockId, to);
    }

    /// ICondition: the Bank asks how much of a lock may move.
    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        Offer storage o = offers[abi.decode(demand, (uint256))];
        if (o.state == 2 && o.lockId == lockId) return toPayee && o.price > paid ? o.price - paid : 0;
        if (o.state == 3 && !toPayee) return total - paid - refunded; // cancelled: the buyer gets it back now
        return 0;
    }

    function offerCount() external view returns (uint256) {
        return offers.length;
    }
}
