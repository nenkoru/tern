// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";

interface IItem1155 {
    function safeTransferFrom(address from, address to, uint256 id, uint256 value, bytes calldata data) external;
    function balanceOf(address owner, uint256 id) external view returns (uint256);
}

interface IItem721 {
    function transferFrom(address from, address to, uint256 id) external;
    function ownerOf(uint256 id) external view returns (address);
}

interface IProbe165 {
    function supportsInterface(bytes4 id) external view returns (bool);
}

/// Decides how much of a lock may move now. Condition contracts are
/// pluggable: anyone can write one; payers choose which to use and payees
/// check it before they rely on a lock.
interface ICondition {
    /// toPayee: a claim for the payee; otherwise an early refund to the payer.
    function check(uint256 lockId, bool toPayee, address caller, bytes calldata demand, bytes calldata fulfillment,
        uint256 total, uint256 paid, uint256 refunded) external view returns (uint256 amount);
}

/// A condition that can also name the payee, for locks opened without one
/// (a pot for a winner not known yet, an open bounty). The Bank asks it at
/// the first claim and then keeps that payee for good.
interface IPayeeCondition {
    function payeeOf(uint256 lockId, bytes calldata demand, bytes calldata fulfillment) external view returns (bytes32);
}

/// Money between agents: the network coin, and any ERC-20 token (a
/// stablecoin, a game currency) held next to it; and things: ERC-1155 and
/// ERC-721 items, held and locked like money (escrow for any asset).
///
/// Primitives, nothing else:
///   balances    deposit, withdraw (owners only)
///   pay         a direct payment, within an agent's limit
///   allowances  an owner lets any contract move its money (custom products)
///   items       depositItem / withdrawItem / lockItem: ERC-1155 or ERC-721
///               units held per owner, locked under any condition (an item
///               for coins: SwapCondition). They go to names, not splits.
///   locks       money set aside for a payee, released by a pluggable
///               condition, refundable to the payer after a deadline.
///               The payee may be left open (the condition names it), and
///               the payee may give money back at any time.
///
/// Escrow with an arbiter, payment on a score, bonds, sales, subscriptions
/// and payment channels (token streaming) are conditions or recipes on top
/// of locks, not part of the Bank. Every movement of an owner's money by an
/// agent counts against the agent's per-epoch limit, including a lock.
contract Bank {
    Registry public immutable registry;

    mapping(bytes32 => uint256) public balance; // owner id => wei
    mapping(bytes32 => mapping(uint64 => uint256)) public spent; // agent id => epoch => wei
    mapping(bytes32 => mapping(address => uint256)) public allowance; // owner id => contract => wei
    // ERC-20 tokens. Agents can't move tokens until their owner sets a limit per token.
    mapping(bytes32 => mapping(address => uint256)) public tokenBalance; // owner id => token => units
    mapping(bytes32 => mapping(address => uint256)) public tokenLimit; // agent id => token => per epoch
    mapping(bytes32 => mapping(address => mapping(uint64 => uint256))) public tokenSpent;
    // Splits: a payee that shares whatever it receives (co-authors, a band, a guild).
    struct Split {
        bytes32[] payees;
        uint16[] bps;
    }
    mapping(bytes32 => Split) internal splits;

    struct Lock {
        bytes32 payer;     // owner id whose money it is
        bytes32 from;      // the owner or agent that created it
        bytes32 payee;
        uint256 total;
        uint256 paid;
        uint256 refunded;
        uint64 refundAfter;
        address condition;
        bytes demand;
        address token; // 0 = the network coin
    }

    Lock[] public locks;
    /// When each lock was opened: a ruling made before its lock existed
    /// doesn't decide it (audit 4 MO-6), and a payee checks hearings against it.
    mapping(uint256 => uint64) public openedAt;
    /// Locks their condition holds: no give-back or release, only what the
    /// condition allows (claim, refundEarly) and the refund after the deadline.
    /// A counted pledge stays in until its campaign resolves (audit 4 MO-2).
    mapping(uint256 => bool) public held;
    /// Items held here: owner id => item contract => item id => units.
    mapping(bytes32 => mapping(address => mapping(uint256 => uint256))) public itemBalance;
    /// Item locks: lock => item id + 1 (0: coins or an ERC-20). Its token is
    /// the item contract, its total the units.
    mapping(uint256 => uint256) public itemOf;
    bool private pulling; // our own depositItem is taking an ERC-1155 in

    event Deposited(bytes32 indexed owner, uint256 amount);
    event Withdrawn(bytes32 indexed owner, address to, uint256 amount);
    event Paid(bytes32 indexed from, bytes32 indexed to, uint256 amount, bytes32 memo);
    event Approved(bytes32 indexed owner, address indexed spender, uint256 amount);
    event Locked(uint256 indexed id, bytes32 indexed from, bytes32 indexed payee, uint256 total, uint64 refundAfter,
        address condition, bytes demand);
    event Claimed(uint256 indexed id, uint256 amount, address by);
    event Refunded(uint256 indexed id, uint256 amount, bool early);
    event Released(uint256 indexed id, uint256 amount);
    event TokenDeposited(bytes32 indexed owner, address indexed token, uint256 amount);
    event TokenWithdrawn(bytes32 indexed owner, address indexed token, address to, uint256 amount);
    event TokenPaid(bytes32 indexed from, bytes32 indexed to, address indexed token, uint256 amount, bytes32 memo);
    event TokenLimitSet(bytes32 indexed agent, address indexed token, uint256 perEpoch);
    event SplitCreated(bytes32 indexed id, bytes32[] payees, uint16[] bps);
    event PayeeSet(uint256 indexed id, bytes32 indexed payee);
    event GivenBack(uint256 indexed id, uint256 amount);
    event ItemDeposited(bytes32 indexed owner, address indexed token, uint256 indexed id, uint256 amount);
    event ItemWithdrawn(bytes32 indexed owner, address indexed token, uint256 indexed id, address to, uint256 amount);

    /// The one contract allowed to collect a fee from a name's balance: the
    /// Inbox, for envelopes (so agents pay message fees within their limit).
    address public immutable feeCollector;

    event FeePaid(bytes32 indexed from, uint256 amount);
    event Accepted(uint256 indexed id, bytes32 indexed payee);

    /// The payee accepted the lock's terms (its condition and who decides).
    /// Rulings may refund the payer early only on accepted locks: a payer can't
    /// name a friendly arbiter after the fact and take the money back. The
    /// payee's own claim accepts too: having taken its share under a ruling it
    /// chose, it can't lose more by that ruling applying to the payer's part
    /// (PROOFREAD-5 F7). A claim by anyone else doesn't: claims are open to
    /// all, and the payer could pick its lowest ruling and pay the payee that.
    mapping(uint256 => bool) public accepted;

    /// A payee accepts (and then delivers) only with time left to dispute: a
    /// lock whose refund deadline is nearer than this can't be accepted, so a
    /// payer can't take the goods and the money back by plain timeout (H1-R).
    uint64 public constant MIN_DISPUTE = 12 hours;

    function acceptLock(uint256 id) external {
        Lock storage l = locks[id];
        require(l.payee != bytes32(0), "no payee yet");
        require(l.refundAfter >= block.timestamp + MIN_DISPUTE, "refund deadline under 12 h away: too short to dispute");
        require(payeeSide(l.payee, msg.sender), "payee only");
        accepted[id] = true;
        emit Accepted(id, l.payee);
    }

    /// who acts for the payee: the payee name, or its owner.
    function payeeSide(bytes32 payee, address who) internal view returns (bool ok) {
        (ok,,) = registry.canAct(payee, who);
        if (!ok) (ok,,) = registry.canAct(registry.ownerOf(payee), who);
    }

    /// For conditions: the parties of a lock as owner ids, and whether the payee accepted.
    function parties(uint256 id) external view returns (bytes32 payerOwner, bytes32 payeeOwner, bool isAccepted) {
        Lock storage l = locks[id];
        payeeOwner = l.payee == bytes32(0) ? bytes32(0) : registry.ownerOf(l.payee);
        if (payeeOwner == bytes32(0)) payeeOwner = l.payee; // a split
        return (l.payer, payeeOwner, accepted[id]);
    }

    constructor(Registry r, address inbox) {
        registry = r;
        feeCollector = inbox;
    }

    /// Called by the Inbox: takes a fee from `from`, acting as `actor` (the
    /// Inbox's caller), with the same checks and agent limits as a payment,
    /// and sends it to `to` (the network's treasury).
    function collectFee(bytes32 from, address actor, uint256 amount, address payable to) external {
        require(msg.sender == feeCollector, "the inbox only");
        debitBy(from, actor, amount);
        (bool sent,) = to.call{value: amount}("");
        require(sent, "send");
        emit FeePaid(from, amount);
    }

    function deposit(bytes32 owner) external payable {
        (, , , , , string memory name) = registry.owners(owner);
        require(bytes(name).length > 0, "no owner");
        balance[owner] += msg.value;
        emit Deposited(owner, msg.value);
    }

    function withdraw(bytes32 owner, address payable to, uint256 amount) external {
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && registry.ownerOf(owner) == owner, "owner only");
        require(balance[owner] >= amount, "balance");
        balance[owner] -= amount;
        (bool sent,) = to.call{value: amount}("");
        require(sent, "send");
        emit Withdrawn(owner, to, amount);
    }

    function debit(bytes32 from, uint256 amount) internal returns (bytes32 owner) {
        return debitBy(from, msg.sender, amount);
    }

    function debitBy(bytes32 from, address actor, uint256 amount) internal returns (bytes32 owner) {
        bool ok;
        bool isAgent;
        (ok, owner, isAgent) = registry.canAct(from, actor);
        require(ok, "not yours");
        if (isAgent) {
            uint64 e = registry.epoch();
            uint256 s = spent[from][e] + amount;
            require(s <= registry.spendLimit(from), "over the agent's limit");
            spent[from][e] = s;
        }
        require(balance[owner] >= amount, "balance");
        balance[owner] -= amount;
    }

    function credit(bytes32 to, uint256 amount) internal {
        creditAsset(to, address(0), amount);
    }

    function creditAsset(bytes32 to, address token, uint256 amount) internal {
        Split storage sp = splits[to];
        if (sp.payees.length > 0) {
            uint256 left = amount;
            for (uint256 i = 1; i < sp.payees.length; i++) {
                uint256 part = amount * sp.bps[i] / 10000;
                left -= part;
                addTo(registry.ownerOf(sp.payees[i]), token, part);
            }
            addTo(registry.ownerOf(sp.payees[0]), token, left); // the first payee takes the rounding
            return;
        }
        bytes32 owner = registry.ownerOf(to);
        require(owner != bytes32(0), "no such recipient");
        addTo(owner, token, amount);
    }

    function addTo(bytes32 owner, address token, uint256 amount) internal {
        if (token == address(0)) balance[owner] += amount;
        else tokenBalance[owner][token] += amount;
    }

    /// Takes an asset from `from` (an owner, or an agent within its limit for that asset).
    function debitAsset(bytes32 from, address token, uint256 amount) internal returns (bytes32 owner) {
        if (token == address(0)) return debit(from, amount);
        bool ok;
        bool isAgent;
        (ok, owner, isAgent) = registry.canAct(from, msg.sender);
        require(ok, "not yours");
        if (isAgent) {
            uint64 e = registry.epoch();
            uint256 s = tokenSpent[from][token][e] + amount;
            require(s <= tokenLimit[from][token], "over the agent's limit for this token");
            tokenSpent[from][token][e] = s;
        }
        require(tokenBalance[owner][token] >= amount, "token balance");
        tokenBalance[owner][token] -= amount;
    }

    function isPayee(bytes32 id) public view returns (bool) {
        return registry.ownerOf(id) != bytes32(0) || splits[id].payees.length > 0;
    }

    // ---- splits

    function createSplit(bytes32[] calldata payees, uint16[] calldata bps) external returns (bytes32 id) {
        require(payees.length >= 2 && payees.length == bps.length && payees.length <= 50, "payees");
        uint256 sum;
        for (uint256 i = 0; i < payees.length; i++) {
            require(registry.ownerOf(payees[i]) != bytes32(0), "payees must be names");
            sum += bps[i];
        }
        require(sum == 10000, "shares must add up to 10000");
        id = keccak256(abi.encode("tern-split", payees, bps));
        if (splits[id].payees.length == 0) {
            splits[id] = Split(payees, bps);
            emit SplitCreated(id, payees, bps);
        }
    }

    function splitOf(bytes32 id) external view returns (bytes32[] memory, uint16[] memory) {
        return (splits[id].payees, splits[id].bps);
    }

    // ---- tokens

    function depositToken(bytes32 owner, address token, uint256 amount) external {
        require(registry.ownerOf(owner) == owner, "no owner");
        require(IERC20(token).transferFrom(msg.sender, address(this), amount), "transfer");
        tokenBalance[owner][token] += amount;
        emit TokenDeposited(owner, token, amount);
    }

    function withdrawToken(bytes32 owner, address token, address to, uint256 amount) external {
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && registry.ownerOf(owner) == owner, "owner only");
        require(tokenBalance[owner][token] >= amount, "token balance");
        tokenBalance[owner][token] -= amount;
        require(IERC20(token).transfer(to, amount), "transfer");
        emit TokenWithdrawn(owner, token, to, amount);
    }

    /// The owner lets one of its agents move up to perEpoch of a token.
    function setTokenLimit(bytes32 agent, address token, uint256 perEpoch) external {
        bytes32 owner = registry.ownerOf(agent);
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && owner != agent, "the agent's owner only");
        tokenLimit[agent][token] = perEpoch;
        emit TokenLimitSet(agent, token, perEpoch);
    }

    function payToken(bytes32 from, bytes32 to, address token, uint256 amount, bytes32 memo) external {
        require(token != address(0), "use pay for coins");
        debitAsset(from, token, amount);
        creditAsset(to, token, amount);
        emit TokenPaid(from, to, token, amount, memo);
    }

    function pay(bytes32 from, bytes32 to, uint256 amount, bytes32 memo) external {
        require(isPayee(to), "no such recipient");
        debit(from, amount);
        credit(to, amount);
        emit Paid(from, to, amount, memo);
    }

    // ---- allowances: any contract can build on the network's money

    function approve(bytes32 owner, address spender, uint256 amount) external {
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && registry.ownerOf(owner) == owner, "owners only");
        allowance[owner][spender] = amount;
        emit Approved(owner, spender, amount);
    }

    function useAllowance(bytes32 owner, uint256 amount) internal {
        require(allowance[owner][msg.sender] >= amount, "allowance");
        require(balance[owner] >= amount, "balance");
        allowance[owner][msg.sender] -= amount;
        balance[owner] -= amount;
    }

    function pull(bytes32 owner, bytes32 to, uint256 amount, bytes32 memo) external {
        useAllowance(owner, amount);
        credit(to, amount);
        emit Paid(owner, to, amount, memo);
    }

    function pullCoins(bytes32 owner, uint256 amount) external {
        useAllowance(owner, amount);
        (bool sent,) = payable(msg.sender).call{value: amount}("");
        require(sent, "send");
        emit Withdrawn(owner, msg.sender, amount);
    }

    // ---- items: ERC-1155 and ERC-721, held per owner

    function is1155(address token) public view returns (bool) {
        return _supports(token, 0xd9b67a26);
    }

    function _supports(address token, bytes4 id) internal view returns (bool) {
        try IProbe165(token).supportsInterface(id) returns (bool y) {
            return y;
        } catch {
            return false;
        }
    }

    /// An item collection: ERC-1155, or ERC-721 (ERC-165 says so). Anything
    /// else (an ERC-20, a no-op contract) is refused (audit 6 IT-4).
    function moveItem(address token, address from, address to, uint256 id, uint256 amount) internal {
        if (is1155(token)) {
            IItem1155(token).safeTransferFrom(from, to, id, amount, "");
        } else {
            require(_supports(token, 0x80ac58cd), "not an item collection (ERC-1155 or ERC-721)");
            require(amount == 1, "an ERC-721 token is one");
            IItem721(token).transferFrom(from, to, id);
        }
    }

    /// Takes items from the caller (who approved the Bank on the collection)
    /// into owner's balance here, once they did arrive.
    function depositItem(bytes32 owner, address token, uint256 id, uint256 amount) external {
        require(registry.ownerOf(owner) == owner && amount > 0, "no owner");
        bool multi = is1155(token);
        uint256 had = multi ? IItem1155(token).balanceOf(address(this), id) : 0;
        pulling = true;
        moveItem(token, msg.sender, address(this), id, amount);
        pulling = false;
        if (multi) require(IItem1155(token).balanceOf(address(this), id) == had + amount, "the items didn't arrive");
        else require(IItem721(token).ownerOf(id) == address(this), "the item didn't arrive");
        itemBalance[owner][token][id] += amount;
        emit ItemDeposited(owner, token, id, amount);
    }

    function withdrawItem(bytes32 owner, address token, uint256 id, address to, uint256 amount) external {
        (bool ok, , bool isAgent) = registry.canAct(owner, msg.sender);
        require(ok && !isAgent && registry.ownerOf(owner) == owner, "owner only");
        require(itemBalance[owner][token][id] >= amount, "item balance");
        itemBalance[owner][token][id] -= amount;
        moveItem(token, address(this), to, id, amount);
        emit ItemWithdrawn(owner, token, id, to, amount);
    }

    /// ERC-1155 sent here directly: data = abi.encode(owner id) credits that owner.
    function onERC1155Received(address, address, uint256 id, uint256 value, bytes calldata data) external returns (bytes4) {
        if (!pulling) {
            require(data.length == 32, "say whose: data = abi.encode(owner id)");
            bytes32 owner = abi.decode(data, (bytes32));
            require(registry.ownerOf(owner) == owner, "no owner");
            itemBalance[owner][msg.sender][id] += value;
            emit ItemDeposited(owner, msg.sender, id, value);
        }
        return this.onERC1155Received.selector;
    }

    /// ERC-1155 batches sent here directly: data = abi.encode(owner id), as above.
    function onERC1155BatchReceived(address, address, uint256[] calldata ids, uint256[] calldata values, bytes calldata data)
        external returns (bytes4) {
        if (!pulling) {
            bytes32 owner = _whose(data);
            for (uint256 i = 0; i < ids.length; i++) {
                itemBalance[owner][msg.sender][ids[i]] += values[i];
                emit ItemDeposited(owner, msg.sender, ids[i], values[i]);
            }
        }
        return this.onERC1155BatchReceived.selector;
    }

    /// An ERC-721 token sent here with safeTransferFrom: data = abi.encode(owner id).
    function onERC721Received(address, address, uint256 id, bytes calldata data) external returns (bytes4) {
        bytes32 owner = _whose(data);
        require(IItem721(msg.sender).ownerOf(id) == address(this), "the item didn't arrive");
        itemBalance[owner][msg.sender][id] += 1;
        emit ItemDeposited(owner, msg.sender, id, 1);
        return this.onERC721Received.selector;
    }

    function _whose(bytes calldata data) internal view returns (bytes32 owner) {
        require(data.length == 32, "say whose: data = abi.encode(owner id)");
        owner = abi.decode(data, (bytes32));
        require(registry.ownerOf(owner) == owner, "no owner");
    }

    function supportsInterface(bytes4 i) external pure returns (bool) {
        return i == 0x01ffc9a7 || i == 0x4e2312e0 || i == 0x150b7a02; // ERC-165, ERC-1155 receiver, ERC-721 receiver
    }

    /// Takes units of an item from `from` (an owner, or an agent within its limit for that collection).
    function debitItem(bytes32 from, address token, uint256 id, uint256 amount) internal returns (bytes32 owner) {
        bool ok;
        bool isAgent;
        (ok, owner, isAgent) = registry.canAct(from, msg.sender);
        require(ok, "not yours");
        if (isAgent) {
            uint64 e = registry.epoch();
            uint256 s = tokenSpent[from][token][e] + amount;
            require(s <= tokenLimit[from][token], "over the agent's limit for this collection");
            tokenSpent[from][token][e] = s;
        }
        require(itemBalance[owner][token][id] >= amount, "item balance");
        itemBalance[owner][token][id] -= amount;
    }

    // ---- locks

    /// A lock of `amount` units of item `item` of collection `token` (held here: depositItem).
    function lockItem(bytes32 from, bytes32 payee, address token, uint256 item, uint256 amount, uint64 refundAfter, address condition,
        bytes calldata demand) external returns (uint256 id) {
        require(token != address(0) && amount > 0, "item");
        require(payee == bytes32(0) || registry.ownerOf(payee) != bytes32(0), "items go to a name, not a split");
        return _lock(from, payee, token, amount, refundAfter, condition, demand, item + 1);
    }

    function lock(bytes32 from, bytes32 payee, uint256 total, uint64 refundAfter, address condition,
        bytes calldata demand) external returns (uint256 id) {
        return _lock(from, payee, address(0), total, refundAfter, condition, demand, 0);
    }

    /// A lock in an ERC-20 token instead of the coin.
    function lockToken(bytes32 from, bytes32 payee, address token, uint256 total, uint64 refundAfter, address condition,
        bytes calldata demand) external returns (uint256 id) {
        require(token != address(0), "use lock for coins");
        return _lock(from, payee, token, total, refundAfter, condition, demand, 0);
    }

    function _lock(bytes32 from, bytes32 payee, address token, uint256 total, uint64 refundAfter, address condition,
        bytes calldata demand, uint256 item) internal returns (uint256 id) {
        require(refundAfter > block.timestamp, "refund time must be in the future");
        // payee zero: open; the condition names the payee at the first claim.
        require(payee == bytes32(0) || isPayee(payee), "no such payee");
        require(condition.code.length > 0, "condition must be a contract");
        require(demand.length <= 4096, "demand too big");
        bytes32 payer = item == 0 ? debitAsset(from, token, total) : debitItem(from, token, item - 1, total);
        id = locks.length;
        itemOf[id] = item;
        locks.push(Lock(payer, from, payee, total, 0, 0, refundAfter, condition, demand, token));
        openedAt[id] = uint64(block.timestamp);
        emit Locked(id, from, payee, total, refundAfter, condition, demand);
    }

    function back(uint256 id, Lock storage l, uint256 amount) internal {
        if (itemOf[id] != 0) itemBalance[l.payer][l.token][itemOf[id] - 1] += amount;
        else addTo(l.payer, l.token, amount);
    }

    /// Out of a lock to its payee: coins and tokens (splits share them), or items (to the payee's owner).
    function out(uint256 id, Lock storage l, uint256 amount) internal {
        if (itemOf[id] == 0) {
            creditAsset(l.payee, l.token, amount);
            return;
        }
        bytes32 o = registry.ownerOf(l.payee);
        require(o != bytes32(0), "items go to a name, not a split");
        itemBalance[o][l.token][itemOf[id] - 1] += amount;
    }

    function remaining(uint256 id) public view returns (uint256) {
        Lock storage l = locks[id];
        return l.total - l.paid - l.refunded;
    }

    /// Anyone may claim for the payee: money only moves to the payee, as the condition allows.
    function claim(uint256 id, bytes calldata fulfillment) external {
        Lock storage l = locks[id];
        if (l.payee == bytes32(0)) {
            bytes32 p = IPayeeCondition(l.condition).payeeOf(id, l.demand, fulfillment);
            require(p != bytes32(0) && isPayee(p), "no payee named");
            l.payee = p;
            emit PayeeSet(id, p);
        }
        uint256 amount = ICondition(l.condition).check(id, true, msg.sender, l.demand, fulfillment, l.total, l.paid, l.refunded);
        uint256 rem = remaining(id);
        if (amount > rem) amount = rem;
        require(amount > 0, "nothing to claim");
        l.paid += amount;
        if (!accepted[id] && payeeSide(l.payee, msg.sender)) { // the payee's own claim accepts (see accepted)
            accepted[id] = true;
            emit Accepted(id, l.payee);
        }
        out(id, l, amount);
        emit Claimed(id, amount, msg.sender);
    }

    /// An early refund to the payer, as the condition allows (an arbiter's ruling, a cancellation).
    function refundEarly(uint256 id, bytes calldata fulfillment) external {
        Lock storage l = locks[id];
        uint256 amount = ICondition(l.condition).check(id, false, msg.sender, l.demand, fulfillment, l.total, l.paid, l.refunded);
        uint256 rem = remaining(id);
        if (amount > rem) amount = rem;
        require(amount > 0, "nothing to refund");
        l.refunded += amount;
        back(id, l, amount);
        emit Refunded(id, amount, true);
    }

    /// After the deadline whatever is left goes back to the payer; payees claim before it.
    function refund(uint256 id) external {
        Lock storage l = locks[id];
        require(block.timestamp > l.refundAfter, "not yet");
        uint256 rem = remaining(id);
        require(rem > 0, "nothing left");
        l.refunded += rem;
        back(id, l, rem);
        emit Refunded(id, rem, false);
    }

    /// The lock's own condition holds it (for good): see `held`.
    function hold(uint256 id) external {
        require(msg.sender == locks[id].condition, "the lock's condition only");
        held[id] = true;
    }

    /// The payer side releases money to the payee voluntarily ("I'm satisfied").
    function release(uint256 id, uint256 amount) external {
        Lock storage l = locks[id];
        require(!held[id], "held by its condition: it moves only as the condition says");
        (bool ok,,) = registry.canAct(l.payer, msg.sender);
        if (!ok) (ok,,) = registry.canAct(l.from, msg.sender);
        require(ok, "payer only");
        require(l.payee != bytes32(0), "no payee yet");
        require(amount > 0 && amount <= remaining(id), "amount");
        l.paid += amount;
        out(id, l, amount);
        emit Released(id, amount);
    }

    /// The payee side gives money back to the payer at any time: an unused
    /// deposit, a bond after the game, a cancelled trip. No waiting for the
    /// deadline, unless the lock's condition holds it (`held`).
    function giveBack(uint256 id, uint256 amount) external {
        Lock storage l = locks[id];
        require(!held[id], "held by its condition: it moves only as the condition says");
        require(l.payee != bytes32(0), "no payee yet");
        (bool ok,,) = registry.canAct(l.payee, msg.sender);
        if (!ok) (ok,,) = registry.canAct(registry.ownerOf(l.payee), msg.sender);
        require(ok, "payee only"); // a split payee can't give back: nobody acts for it alone
        require(amount > 0 && amount <= remaining(id), "amount");
        l.refunded += amount;
        back(id, l, amount);
        emit GivenBack(id, amount);
    }

    function lockCount() external view returns (uint256) {
        return locks.length;
    }
}
