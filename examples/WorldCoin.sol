// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// A world's own currency: a plain OpenZeppelin ERC-20, the whole supply to
/// whoever deploys it. The Bank takes it like any ERC-20 (--token ADDRESS).
contract WorldCoin is ERC20 {
    constructor(string memory name_, string memory symbol_, uint256 supply) ERC20(name_, symbol_) {
        _mint(msg.sender, supply * 10 ** decimals());
    }
}
