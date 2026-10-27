// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Bank.sol";

/// Two Bank locks swapped atomically: an item for coins, a token for an
/// item, an item for an item. No arbiter and no offer book: the offer and the
/// answer are ordinary locks under this condition.
///
///   the offer   a lock of what one side gives (Bank.lockItem, lock or
///               lockToken), payee: who may take it (0: anyone; the answer's
///               payer then), demand abi.encode(Want): what it wants back
///   the answer  a lock of exactly that, to Want.payTo, demand abi.encode(offer lock id)
///   settle      anyone, once both exist: each lock pays its payee in the same
///               transaction (Swapped). Until then either side takes its lock
///               back at once (Bank.refundEarly, the payer's side), or after
///               its refund time.
contract SwapCondition is ICondition, IPayeeCondition {
    Bank public immutable bank;

    struct Want {
        address token; // 0: the coin; an ERC-20; or an item collection
        uint256 item; // item id + 1 when it wants an item; 0: coins or tokens
        uint256 amount;
        bytes32 payTo; // the answer's payee: the offerer, or a split for coins
    }

    mapping(uint256 => uint256) public matched; // lock => the other lock + 1

    event Swapped(uint256 indexed offer, uint256 indexed answer);

    constructor(Bank b) {
        bank = b;
    }

    function wantOf(uint256 offer) public view returns (Want memory) {
        (, , , , , , , , bytes memory demand,) = bank.locks(offer);
        return abi.decode(demand, (Want));
    }

    function settle(uint256 offer, uint256 answer) external {
        require(matched[offer] == 0 && matched[answer] == 0, "already swapped");
        (, , bytes32 oPayee, uint256 oTotal, uint256 oPaid, uint256 oRefunded, , address oCond, bytes memory oDemand,) = bank.locks(offer);
        (bytes32 aPayer, , bytes32 aPayee, uint256 aTotal, uint256 aPaid, uint256 aRefunded, , address aCond, bytes memory aDemand,
            address aToken) = bank.locks(answer);
        require(oCond == address(this) && aCond == address(this), "not swap locks");
        require(oDemand.length == 128 && keccak256(aDemand) == keccak256(abi.encode(offer)), "the answer is for another offer");
        Want memory w = abi.decode(oDemand, (Want));
        require(aToken == w.token && bank.itemOf(answer) == w.item && aTotal == w.amount && aPayee == w.payTo, "not what the offer wants");
        require(oPaid == 0 && oRefunded == 0 && aPaid == 0 && aRefunded == 0 && oTotal > 0, "a lock moved already");
        require(oPayee == bytes32(0) || oPayee == aPayer || bank.registry().ownerOf(oPayee) == aPayer, "reserved for someone else");
        matched[offer] = answer + 1;
        matched[answer] = offer + 1;
        bank.claim(offer, abi.encode(answer));
        bank.claim(answer, "");
        emit Swapped(offer, answer);
    }

    /// An open offer goes to the answer's payer.
    function payeeOf(uint256 lockId, bytes calldata, bytes calldata fulfillment) external view returns (bytes32 p) {
        uint256 answer = abi.decode(fulfillment, (uint256));
        require(matched[lockId] == answer + 1, "not swapped with that lock");
        (p, , , , , , , , ,) = bank.locks(answer);
    }

    function check(uint256 lockId, bool toPayee, address caller, bytes calldata, bytes calldata, uint256 total, uint256 paid,
        uint256 refunded) external view returns (uint256) {
        require(msg.sender == address(bank), "the bank asks");
        if (matched[lockId] != 0) return toPayee ? total - paid - refunded : 0;
        require(!toPayee, "not swapped yet");
        (bytes32 payer, bytes32 from, , , , , , , ,) = bank.locks(lockId);
        Registry r = bank.registry();
        (bool ok,,) = r.canAct(payer, caller);
        if (!ok) (ok,,) = r.canAct(from, caller);
        require(ok, "only the payer's side takes an unswapped lock back");
        return total - paid - refunded;
    }
}
