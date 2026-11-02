// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {IERC1155} from "@openzeppelin/contracts/token/ERC1155/IERC1155.sol";
import {ERC1155Holder} from "@openzeppelin/contracts/token/ERC1155/utils/ERC1155Holder.sol";

/// A tiny portal between worlds: an Items token sent here is locked, and a
/// Visa event says who travels where. The other world reads the event and
/// issues its copy; the keeper (the deployer: the other side's bridge) gives
/// the token back when the traveller returns. Just the pattern, unaudited.
contract Portal is ERC1155Holder {
    address public immutable items;
    address public immutable keeper;
    uint256 public visas;

    struct Stay {
        address traveller;
        uint256 item;
        uint256 amount;
        bool back;
    }

    mapping(uint256 => Stay) public stays;

    event Visa(uint256 indexed visa, address indexed traveller, uint256 indexed item, uint256 amount, string destination);
    event Returned(uint256 indexed visa, address traveller, uint256 item, uint256 amount);

    constructor(address items_) {
        items = items_;
        keeper = msg.sender;
    }

    /// Items.safeTransferFrom(you, portal, item, amount, "destination") lands here.
    function onERC1155Received(address, address from, uint256 id, uint256 amount, bytes memory data)
        public
        override
        returns (bytes4)
    {
        require(msg.sender == items, "only the network's Items");
        uint256 v = ++visas;
        stays[v] = Stay(from, id, amount, false);
        emit Visa(v, from, id, amount, string(data));
        return this.onERC1155Received.selector;
    }

    function onERC1155BatchReceived(address, address, uint256[] memory, uint256[] memory, bytes memory)
        public
        pure
        override
        returns (bytes4)
    {
        revert("one item at a time");
    }

    function giveBack(uint256 v) external {
        require(msg.sender == keeper, "keeper only");
        Stay storage s = stays[v];
        require(s.amount > 0 && !s.back, "no such stay");
        s.back = true;
        IERC1155(items).safeTransferFrom(address(this), s.traveller, s.item, s.amount, "");
        emit Returned(v, s.traveller, s.item, s.amount);
    }
}
