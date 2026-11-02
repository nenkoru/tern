// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Bank.sol";
import "./Registry.sol";
import "./Commitments.sol";
import {IEAS, Attestation} from "@ethereum-attestation-service/eas-contracts/contracts/IEAS.sol";

/// A standard set of conditions for Bank locks. Anyone can deploy others.

/// Released by an EAS attestation from a chosen party (a verifier, an
/// arbiter, the buyer). Schema: "bytes32 subject,string kind,bytes data,int64 score".
///   mode 0 (threshold): the payee gets everything once score >= minScore.
///   mode 1 (split):     score is the payee's share in basis points (0..10000);
///                       the rest may be refunded early. An arbiter's ruling.
///   mode 2 (award):     like split, and the attestation names the payee: its
///                       data starts with the payee's name id (32 bytes). For
///                       locks with an open payee: a pot for the winner of a
///                       game, an open bounty for whoever solves it first.
/// The attestation's subject must equal the demand's subject, so one ruling
/// can't be reused for another deal. A ruling (modes 1 and 2) also names its
/// lock: the attestation's recipient is rulingFor(bank, lockId), so a ruling
/// made for one lock can't decide another with the same subject and decider
/// (audit 4 MO-6). Mode 0 needn't: a verifier's score is about the work, and
/// it only ever pays the payee.
interface ILockParties {
    function parties(uint256 id) external view returns (bytes32 payerOwner, bytes32 payeeOwner, bool isAccepted);
}

interface ILockOpened {
    function openedAt(uint256 id) external view returns (uint64);
}

/// The recipient a ruling on a lock carries (EAS's recipient field, otherwise
/// unused by Tern claims): rulings for AttestationCondition (modes 1, 2),
/// PanelCondition and OptimisticCondition count only on the lock they name.
function rulingFor(address bank, uint256 lockId) pure returns (address) {
    return address(uint160(uint256(keccak256(abi.encode(bytes32("tern-ruling"), bank, lockId)))));
}

contract AttestationCondition is ICondition, IPayeeCondition {
    IEAS public immutable eas;
    Registry public immutable registry;

    constructor(IEAS e, Registry r) {
        eas = e;
        registry = r;
    }

    struct Demand {
        bytes32 attester; // a Tern name id
        bytes32 schema;
        bytes32 subject;
        bytes32 kindHash;
        int64 minScore;
        uint8 mode;
    }

    /// The valid claim behind a fulfillment: checks attester, subject, kind.
    /// Rulings (modes 1 and 2) count only if made for this lock (they name it,
    /// rulingFor) since it was opened, so a ruling from another deal on the
    /// same subject can't decide this one (audit 4 MO-6). Mode 0 (pay on a
    /// verifier's score) may use any attestation: it only ever pays the payee.
    function verified(uint256 lockId, bytes calldata demand, bytes calldata fulfillment)
        internal view returns (Demand memory d, bytes memory data, int64 score) {
        d = abi.decode(demand, (Demand));
        bytes32 uid = abi.decode(fulfillment, (bytes32));
        Attestation memory a = eas.getAttestation(uid);
        require(a.schema == d.schema && a.revocationTime == 0, "no valid attestation");
        require(d.mode == 0 || a.time >= ILockOpened(msg.sender).openedAt(lockId), "a ruling made before this lock was opened");
        require(d.mode == 0 || a.recipient == rulingFor(msg.sender, lockId), "a ruling made for another lock");
        require(a.expirationTime == 0 || a.expirationTime > block.timestamp, "attestation expired");
        (bool ok,,) = registry.canAct(d.attester, a.attester);
        require(ok, "attested by someone else");
        bytes32 subject;
        string memory kind;
        (subject, kind, data, score) = abi.decode(a.data, (bytes32, string, bytes, int64));
        require(subject == d.subject && keccak256(bytes(kind)) == d.kindHash, "attestation is about something else");
    }

    /// Nobody decides in their own favour: the payer's side can't refund
    /// itself, and an award can't name its own decider's side.
    function fair(uint256 lockId, bool toPayee, Demand memory d) internal view {
        bytes32 decider = registry.ownerOf(d.attester);
        (bytes32 payerOwner, bytes32 payeeOwner, bool acc) = ILockParties(msg.sender).parties(lockId);
        if (toPayee) {
            require(d.mode != 2 || decider != payeeOwner, "a decider can't award itself");
        } else {
            require(decider != payerOwner, "the payer can't rule its own refund");
            require(d.mode == 2 || acc, "the payee hasn't accepted this lock's terms: no early refund by ruling");
        }
    }

    /// Who decides a lock with this demand, and that name's owner: what a payee's
    /// client checks against its own list of arbiters before accepting (H1-R).
    /// Owner ids can't tell a stranger from the other party's second name.
    function deciderOf(bytes calldata demand) external view returns (bytes32 attester, bytes32 owner, uint8 mode) {
        Demand memory d = abi.decode(demand, (Demand));
        return (d.attester, registry.ownerOf(d.attester), d.mode);
    }

    function payeeOf(uint256 lockId, bytes calldata demand, bytes calldata fulfillment) external view returns (bytes32 p) {
        (Demand memory d, bytes memory data,) = verified(lockId, demand, fulfillment);
        require(d.mode == 2 && data.length >= 32, "this claim names no payee");
        assembly {
            p := mload(add(data, 32))
        }
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata fulfillment, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        (Demand memory d,, int64 score) = verified(lockId, demand, fulfillment);
        if (d.mode != 0) fair(lockId, toPayee, d);
        if (d.mode == 0) {
            if (!toPayee || score < d.minScore) return 0;
            return total - paid - refunded;
        }
        require(score >= 0 && score <= 10000, "share in basis points");
        uint256 payeeShare = total * uint256(int256(score)) / 10000;
        if (toPayee) return payeeShare > paid ? payeeShare - paid : 0;
        uint256 payerShare = total - payeeShare;
        return payerShare > refunded ? payerShare - refunded : 0;
    }
}

/// A payment channel: the payer signs cumulative vouchers off chain ("I owe
/// you N in total under lock L"); the payee claims the latest whenever it
/// likes. Streaming tokens, metered APIs, per-minute help.
contract VoucherCondition is ICondition {
    /// Bound to this chain and this condition (audit 4 MO-15): a voucher signed for a
    /// testnet that shares addresses with mainnet pays nothing on mainnet.
    function voucherDigest(address bank, uint256 lockId, uint256 cumulative) public view returns (bytes32) {
        return keccak256(abi.encodePacked("\x19Ethereum Signed Message:\n32",
            keccak256(abi.encode("tern-voucher", block.chainid, address(this), bank, lockId, cumulative))));
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata fulfillment, uint256 total,
        uint256 paid, uint256) external view returns (uint256) {
        if (!toPayee) return 0;
        address signer = abi.decode(demand, (address));
        (uint256 cumulative, bytes memory sig) = abi.decode(fulfillment, (uint256, bytes));
        require(sig.length == 65, "signature");
        bytes32 r;
        bytes32 s;
        uint8 v;
        assembly {
            r := mload(add(sig, 32))
            s := mload(add(sig, 64))
            v := byte(0, mload(add(sig, 96)))
        }
        if (v < 27) v += 27;
        require(ecrecover(voucherDigest(msg.sender, lockId, cumulative), v, r, s) == signer, "bad voucher");
        if (cumulative > total) cumulative = total;
        return cumulative > paid ? cumulative - paid : 0;
    }
}

/// A fixed amount per period, from a start time: subscriptions, rent,
/// salaries. The payee claims what has accrued; the payer may cancel at
/// any time and take back what has not accrued yet.
contract RateCondition is ICondition {
    Registry public immutable registry;

    constructor(Registry r) {
        registry = r;
    }

    struct Demand {
        uint256 perPeriod;
        uint64 start;
        uint64 period;
        bytes32 payer; // who may cancel (an owner or agent id)
    }

    function accrued(Demand memory d, uint256 total) internal view returns (uint256) {
        if (block.timestamp < d.start) return 0;
        uint256 a = d.perPeriod * ((block.timestamp - d.start) / d.period + 1);
        return a > total ? total : a;
    }

    function check(uint256, bool toPayee, address caller, bytes calldata demand, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        Demand memory d = abi.decode(demand, (Demand));
        require(d.period > 0, "period");
        uint256 a = accrued(d, total - refunded);
        if (toPayee) return a > paid ? a - paid : 0;
        (bool ok,,) = registry.canAct(d.payer, caller);
        if (!ok) (ok,,) = registry.canAct(registry.ownerOf(d.payer), caller);
        require(ok, "only the payer cancels");
        uint256 left = total - refunded;
        return left > a ? left - a : 0;
    }
}

/// A reveal bond: money that comes back once a commitment is revealed, and
/// goes to the payee if it isn't revealed in time. Makes commit-reveal games,
/// sealed bids and secret ballots safe without an arbiter: walking away from
/// a bad hand costs the bond.
contract RevealCondition is ICondition {
    Commitments public immutable commitments;

    constructor(Commitments c) {
        commitments = c;
    }

    function check(uint256, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        uint256 cid = abi.decode(demand, (uint256));
        (, , , uint64 revealBy, bool revealed, , , ) = commitments.all(cid);
        uint256 rem = total - paid - refunded;
        if (toPayee) return (!revealed && block.timestamp > revealBy) ? rem : 0;
        return revealed ? rem : 0;
    }
}

/// A hash lock (HTLC, as in BIP-199 and cross-chain atomic swaps): the payee
/// gets everything by presenting a preimage of the demand's hash before the
/// lock's refund time; after it, only the payer's refund (Bank.refund). No
/// decider and no early refund, so the payee needn't accept the lock (H1-R is
/// about rulings); it checks the hash, the amount and the time left instead.
///
/// The preimage is published here when presented (`Revealed`, keyed by both
/// hashes), so a counterparty on another network reads it and claims the
/// matching lock there. Claim through `claim` (it reveals, then claims), or
/// `reveal` first and then Bank.claim. Preimages are exactly 32 bytes, as in
/// atomic swap scripts, so a preimage valid on one chain is valid on the other.
///   demand       abi.encode(bytes32 hash, uint8 algo)  algo 0 = sha256, 1 = keccak256
///   fulfillment  the preimage
contract HashlockCondition is ICondition {
    Bank public immutable bank;
    mapping(bytes32 => uint64) public revealedAt; // hash (either algo) => first revealed here

    event Revealed(bytes32 indexed sha, bytes32 indexed keccak, bytes preimage);

    constructor(Bank b) {
        bank = b;
    }

    function hashOf(bytes calldata preimage, uint8 algo) public pure returns (bytes32) {
        require(preimage.length == 32, "the preimage is 32 bytes");
        require(algo < 2, "algo: 0 sha256, 1 keccak256");
        return algo == 0 ? sha256(preimage) : keccak256(preimage);
    }

    /// Publishes a preimage (anyone may; it only lets payees claim).
    function reveal(bytes calldata preimage) public {
        bytes32 s = hashOf(preimage, 0);
        bytes32 k = keccak256(preimage);
        if (revealedAt[s] != 0) return;
        revealedAt[s] = uint64(block.timestamp);
        revealedAt[k] = uint64(block.timestamp);
        emit Revealed(s, k, preimage);
    }

    /// Reveals the preimage and claims the lock for its payee, in one transaction.
    function claim(uint256 lockId, bytes calldata preimage) external {
        reveal(preimage);
        bank.claim(lockId, preimage);
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata fulfillment, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        if (!toPayee) return 0; // the payer's money comes back only by Bank.refund, after the refund time
        (bytes32 h, uint8 algo) = abi.decode(demand, (bytes32, uint8));
        require(hashOf(fulfillment, algo) == h, "wrong preimage");
        require(revealedAt[h] != 0, "reveal the preimage first (HashlockCondition.claim does both)");
        (, , , , , , uint64 refundAfter, , , ) = Bank(msg.sender).locks(lockId);
        require(block.timestamp <= refundAfter, "past the refund time: the payer's now");
        return total - paid - refunded;
    }
}

/// All or nothing (an assurance contract, as Kickstarter): backers lock
/// pledges to a campaign's creator (a person, an agency, a community's name);
/// the creator collects them all once the registered pledges reach the goal,
/// at once, without waiting for the deadline; if the deadline passes below
/// the goal, each backer takes its pledge back. No early refunds and no
/// give-backs: registering a pledge holds its lock (Bank.hold), so it moves
/// only to the creator once the goal is met, or back after a missed deadline,
/// and nobody can push the total over the goal and pull it back (audit 4
/// MO-2). The counted total is therefore a plain running sum, with no cap on
/// how many pledges count (dust pledges can't fill a campaign). A pledge
/// counts once it is registered here (anyone may register it; the client
/// does, right after the lock), and only if its refund time leaves the
/// creator COLLECT_WINDOW after the deadline.
///   demand  abi.encode(uint256 campaign, uint256 goal, uint64 deadline)
contract PledgeCondition is ICondition {
    Bank public immutable bank;
    uint64 public constant COLLECT_WINDOW = 7 days;

    struct Campaign {
        bytes32 creator; // who collects: a name, a split or a community
        bytes32 by; // who announced it
        uint256 goal;
        uint64 deadline;
        uint64 reachedAt; // 0 until the goal is met
        uint32 backers;
        uint256 pledged;
        string title;
        bytes32 terms; // a URI's or a text's hash
    }

    Campaign[] public campaigns;
    mapping(uint256 => bool) public registered; // lock => counted (and held)
    mapping(uint256 => mapping(bytes32 => bool)) public backed; // campaign => payer

    event CampaignCreated(uint256 indexed campaign, bytes32 indexed creator, bytes32 indexed by, uint256 goal, uint64 deadline,
        string title, bytes32 terms);
    event Pledged(uint256 indexed campaign, uint256 indexed lockId, bytes32 indexed backer, uint256 amount, uint256 pledged);
    event GoalReached(uint256 indexed campaign, uint256 pledged);

    constructor(Bank b) {
        bank = b;
    }

    function create(bytes32 by, bytes32 creator, uint256 goal, uint64 deadline, string calldata title, bytes32 terms)
        external returns (uint256 id) {
        (bool ok,,) = bank.registry().canAct(by, msg.sender);
        require(ok, "not yours");
        require(bank.isPayee(creator), "no such creator");
        require(goal > 0 && deadline > block.timestamp, "goal or deadline");
        id = campaigns.length;
        campaigns.push(Campaign(creator, by, goal, deadline, 0, 0, 0, title, terms));
        emit CampaignCreated(id, creator, by, goal, deadline, title, terms);
    }

    function demandOf(uint256 id) public view returns (bytes memory) {
        return abi.encode(id, campaigns[id].goal, campaigns[id].deadline);
    }

    /// Counts a pledge: a coin lock under this condition and this campaign's
    /// demand, to its creator, untouched, before the deadline, refundable no
    /// sooner than COLLECT_WINDOW after it, and not the creator's own money
    /// (audit 4 MO-7). Each lock counts once, and is held from now on: until
    /// the campaign resolves its money stays in, so the running sum is what
    /// the counted pledges hold.
    function register(uint256 lockId) external {
        require(!registered[lockId], "already counted");
        (bytes32 payer, , bytes32 payee, uint256 total, uint256 paid, uint256 refunded, uint64 refundAfter, address cond,
            bytes memory demand, address token) = bank.locks(lockId);
        require(cond == address(this), "not a pledge: another condition");
        require(demand.length == 96, "not a pledge's demand");
        uint256 id = abi.decode(demand, (uint256));
        require(id < campaigns.length && keccak256(demand) == keccak256(demandOf(id)), "not this campaign's demand");
        Campaign storage k = campaigns[id];
        require(payee == k.creator && token == address(0), "not to the creator, or not in coins");
        require(paid == 0 && refunded == 0, "already moved");
        require(block.timestamp <= k.deadline, "the campaign is over");
        require(refundAfter >= k.deadline + COLLECT_WINDOW, "refund time too early: the creator needs time to collect");
        Registry r = bank.registry();
        require(payer != r.ownerOf(k.creator) && payer != r.ownerOf(k.by), "the creator's own pledge doesn't count");
        registered[lockId] = true;
        bank.hold(lockId);
        k.pledged += total;
        if (!backed[id][payer]) {
            backed[id][payer] = true;
            k.backers++;
        }
        emit Pledged(id, lockId, payer, total, k.pledged);
        if (k.reachedAt == 0 && k.pledged >= k.goal) {
            k.reachedAt = uint64(block.timestamp);
            emit GoalReached(id, k.pledged);
        }
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total, uint256 paid,
        uint256 refunded) external view returns (uint256) {
        uint256 id = abi.decode(demand, (uint256));
        require(id < campaigns.length && keccak256(demand) == keccak256(demandOf(id)), "not this campaign's demand");
        Campaign storage k = campaigns[id];
        if (toPayee) require(k.reachedAt != 0 && registered[lockId], "the goal isn't reached, or this pledge was never counted");
        else require(k.reachedAt == 0 && block.timestamp > k.deadline, "no refund: the goal was met, or the campaign is open");
        return total - paid - refunded;
    }

    function campaignCount() external view returns (uint256) {
        return campaigns.length;
    }
}

// OptimisticCondition and PanelCondition: Ruling.sol (one RulingBase).
