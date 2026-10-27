// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Bank.sol";
import "./Registry.sol";
import {IEAS, Attestation} from "@ethereum-attestation-service/eas-contracts/contracts/IEAS.sol";
import {ILockParties, ILockOpened, rulingFor} from "./Conditions.sol";

/// Locks decided by rulings: one arbiter or a panel, k of n, an aggregation
/// rule, an optional hearing, an optional dispute window with a dispute bond.
///
/// RulingBase is the one implementation. RulingCondition takes every choice
/// as a demand parameter; PanelCondition (a hearing, then k of n, lower
/// median) and OptimisticCondition (pay unless the payer disputes, then k of
/// n, lower median) are the same code with their choices fixed, kept for the
/// locks and clients that name them.
///
/// How a decision is made (audit 6 RU-1): each decider attests its ruling in
/// EAS and records it here (`record`, the decider itself). The first k
/// distinct deciders to record decide; a decider may record a newer ruling,
/// which replaces its own, until the k-th decider records; then the decision
/// is frozen (`decided`). Claims and refunds apply that decision: whoever
/// submits them picks nothing (the fulfillment is ignored).
///
/// Floors, the same for all three (they protect people, so no demand turns
/// them off):
///   - a ruling counts only on the lock it names (its EAS recipient is
///     rulingFor(bank, lockId), audit 4 MO-6), made after the lock opened,
///     the hearing and the dispute;
///   - deciders on the payer's or the payee's side don't count (a member of
///     a split payee is on the payee's side): nobody judges their own case;
///     one seat per owner, however often a decider is listed;
///   - a ruling refunds the payer early only on a lock its payee accepted
///     (Bank.acceptLock, which needs 12 h left);
///   - a dispute leaves the deciders MIN_DECIDE (12 h) before the refund
///     time, counted from the end of the hearing: a payer can't dispute at
///     the last minute and take everything back by timeout.
abstract contract RulingBase {
    IEAS public immutable eas;
    Registry public immutable registry;
    Bank public immutable bank;
    uint64 public constant MIN_DECIDE = 12 hours;

    /// Aggregation rules over the k counted rulings (each a payee share in bps).
    uint8 public constant LOWER_MEDIAN = 0; // in doubt, for the payer
    uint8 public constant MEDIAN = 1; // the middle; for an even k the mean of the two middles, rounded down
    uint8 public constant MEAN = 2; // rounded down
    uint8 public constant UNANIMOUS = 3; // the least share all k agree to (any dissent lowers it)
    uint8 public constant THRESHOLD = 4; // all to the payee if at least `quota` of the k say >= 50%, else all to the payer

    /// What a ruling must match.
    struct Ask {
        bytes32[] deciders;
        uint8 k;
        bytes32 schema;
        bytes32 subject;
        bytes32 kindHash;
        uint64 since; // rulings attested before this don't count
        address tag; // rulingFor(bank, lockId)
        bytes32 payerOwner;
        bytes32 payeeOwner;
    }

    /// A recorded ruling: whose (the decider's owner id), when it was attested, the share.
    struct Seat {
        bytes32 who;
        uint64 at;
        int64 score;
    }

    mapping(uint256 => Seat[]) internal seats; // lock => recorded rulings, in the order deciders first recorded
    mapping(uint256 => bool) public decided; // lock => k rulings recorded: the decision is frozen

    event Recorded(uint256 indexed lockId, bytes32 indexed decider, bytes32 uid, int64 score, uint256 seats, bool decided);

    constructor(IEAS e, Registry r, Bank b) {
        eas = e;
        registry = r;
        bank = b;
    }

    /// What a ruling on this lock must match now (reverts if no ruling can count yet).
    function _askOf(uint256 lockId) internal view virtual returns (Ask memory);

    /// A ruling's attester, score and time if it counts (schema, subject,
    /// kind, lock, time, not revoked or expired, 0..10000); else attester 0.
    function _ruling(Ask memory q, bytes32 uid) internal view returns (address, int64, uint64) {
        Attestation memory a = eas.getAttestation(uid);
        if (a.schema != q.schema || a.revocationTime != 0 || a.time < q.since || a.recipient != q.tag) return (address(0), 0, 0);
        if (a.expirationTime != 0 && a.expirationTime <= block.timestamp) return (address(0), 0, 0);
        (bytes32 subject, string memory kind, , int64 score) = abi.decode(a.data, (bytes32, string, bytes, int64));
        if (subject != q.subject || keccak256(bytes(kind)) != q.kindHash || score < 0 || score > 10000) return (address(0), 0, 0);
        return (a.attester, score, a.time);
    }

    /// The seat an attester takes: the owner of a listed decider it acts for,
    /// not on a party's side (a split payee's members are its side).
    function _seatOf(Ask memory q, address attester) internal view returns (bytes32) {
        bytes32[] memory members;
        if (q.payeeOwner != bytes32(0) && registry.ownerOf(q.payeeOwner) == bytes32(0)) (members,) = bank.splitOf(q.payeeOwner);
        for (uint256 j = 0; j < q.deciders.length; j++) {
            (bool ok,,) = registry.canAct(q.deciders[j], attester);
            if (!ok) continue;
            bytes32 side = registry.ownerOf(q.deciders[j]);
            if (side == bytes32(0) || side == q.payerOwner || side == q.payeeOwner) continue;
            bool member;
            for (uint256 m = 0; m < members.length && !member; m++) member = registry.ownerOf(members[m]) == side;
            if (!member) return side;
        }
        revert("not one of the lock's deciders (or on a party's side)");
    }

    /// A decider records its ruling (attested for this lock): it takes a seat,
    /// or replaces its own earlier ruling, until k deciders have recorded.
    function record(uint256 lockId, bytes32 uid) external {
        require(!decided[lockId], "decided: the k rulings are in");
        Ask memory q = _askOf(lockId);
        require(q.k > 0 && q.k <= q.deciders.length, "k");
        (address attester, int64 score, uint64 at) = _ruling(q, uid);
        require(attester != address(0), "not a ruling that counts on this lock (schema, subject, kind, lock, time)");
        require(attester == msg.sender, "a decider records its own ruling");
        bytes32 who = _seatOf(q, attester);
        Seat[] storage s = seats[lockId];
        uint256 i;
        while (i < s.length && s[i].who != who) i++;
        if (i < s.length) {
            require(at >= s[i].at, "an older ruling than the one recorded");
            s[i].at = at;
            s[i].score = score;
        } else {
            s.push(Seat(who, at, score));
            if (s.length == q.k) decided[lockId] = true;
        }
        emit Recorded(lockId, who, uid, score, s.length, decided[lockId]);
    }

    /// The recorded rulings: (decider, score) in the order they first recorded.
    function seatsOf(uint256 lockId) external view returns (Seat[] memory) {
        return seats[lockId];
    }

    /// The decision's k shares, sorted (reverts until k deciders recorded).
    function _decision(uint256 lockId, string memory short) internal view returns (int64[] memory s) {
        require(decided[lockId], short);
        Seat[] storage x = seats[lockId];
        uint256 n = x.length;
        s = new int64[](n);
        for (uint256 i = 0; i < n; i++) {
            int64 v = x[i].score;
            uint256 j = i;
            while (j > 0 && s[j - 1] > v) { // insertion sort (k is small)
                s[j] = s[j - 1];
                j--;
            }
            s[j] = v;
        }
    }

    /// The payee's share in bps from sorted rulings, by rule.
    function aggregate(int64[] memory s, uint8 rule, uint8 quota) public pure returns (uint256) {
        uint256 n = s.length;
        require(n > 0, "no rulings");
        if (rule == LOWER_MEDIAN) return uint256(int256(s[(n - 1) / 2]));
        if (rule == MEDIAN) return n % 2 == 1 ? uint256(int256(s[n / 2])) : (uint256(int256(s[n / 2 - 1])) + uint256(int256(s[n / 2]))) / 2;
        if (rule == MEAN) {
            uint256 t;
            for (uint256 i = 0; i < n; i++) t += uint256(int256(s[i]));
            return t / n;
        }
        if (rule == UNANIMOUS) return uint256(int256(s[0]));
        require(rule == THRESHOLD && quota > 0 && quota <= n, "rule");
        uint256 yes;
        for (uint256 i = 0; i < n; i++) if (s[i] >= 5000) yes++;
        return yes >= quota ? 10000 : 0;
    }

    /// The amount a share lets move now (toPayee: claim; else an early refund).
    function _move(bool toPayee, bool acc, uint256 shareBps, uint256 total, uint256 paid, uint256 refunded) internal pure returns (uint256) {
        require(toPayee || acc, "the payee hasn't accepted this lock's terms: no early refund by ruling");
        uint256 payeeShare = total * shareBps / 10000;
        if (toPayee) return payeeShare > paid ? payeeShare - paid : 0;
        uint256 payerShare = total - payeeShare;
        return payerShare > refunded ? payerShare - refunded : 0;
    }

    function _bankLock(uint256 lockId) internal view returns (bytes32 payer, bytes32 from, bytes32 payee, uint256 rem,
        uint64 refundAfter, address cond, bytes memory demand) {
        uint256 total;
        uint256 paid;
        uint256 refunded;
        (payer, from, payee, total, paid, refunded, refundAfter, cond, demand,) = bank.locks(lockId);
        rem = total - paid - refunded;
    }

    /// The lock's demand, if this condition holds it.
    function _demandOf(uint256 lockId) internal view returns (bytes memory demand) {
        address cond;
        (,,,,,,, cond, demand,) = bank.locks(lockId);
        require(cond == address(this), "not this condition's lock");
    }

    function _ask(uint256 lockId, bytes32[] memory deciders, uint8 k, bytes32 schema, bytes32 subject, bytes32 kindHash, uint64 since)
        internal view returns (Ask memory) {
        (bytes32 payerOwner, bytes32 payeeOwner,) = ILockParties(address(bank)).parties(lockId);
        uint64 opened = ILockOpened(address(bank)).openedAt(lockId);
        if (opened > since) since = opened;
        return Ask(deciders, k, schema, subject, kindHash, since, rulingFor(address(bank), lockId), payerOwner, payeeOwner);
    }
}

/// The general ruling condition. Every choice is a demand parameter:
///   deciders + k     one arbiter (k = 1 of 1) or a panel (k of n)
///   rule, quota      how the k shares combine (RulingBase: lower median,
///                    median, mean, unanimous, k-of-n threshold)
///   hearingUntil     rulings before it don't count (both sides heard first)
///   windowEnds       0: rulings decide at once (a panel). Otherwise pay
///                    unless disputed: the payee is paid after windowEnds
///                    unless someone disputed within it; then the deciders.
///   disputeBond      coin a disputer puts up with the dispute (0: none; with
///                    anyoneDisputes it must be more). Once the lock is
///                    settled the bond goes to the payee's owner in proportion
///                    to the decided share, the rest back to the disputer; all
///                    of it to the payee if the deciders never decided (a
///                    dispute that only blocked the payment).
///   anyoneDisputes   false: the payer's side only; true: anyone (an assertion
///                    anyone may challenge, UMA-style), always with the bond.
///   demand       abi.encode(Demand)
///   fulfillment  ignored: the deciders' recorded rulings decide (RulingBase)
contract RulingCondition is RulingBase, ICondition {
    struct Demand {
        bytes32[] deciders;
        uint8 k;
        uint8 rule;
        uint8 quota;
        bytes32 schema;
        bytes32 subject;
        bytes32 kindHash;
        uint64 hearingUntil;
        uint64 windowEnds;
        uint256 disputeBond;
        bool anyoneDisputes;
    }

    struct Dispute {
        uint64 at;
        bool bondSettled;
        address by;
        uint256 bond;
    }

    mapping(uint256 => Dispute) public disputes; // lock => its dispute (at 0: none)

    event Disputed(uint256 indexed lockId, bytes32 indexed subject, address by, uint256 bond, string reason);
    event BondSettled(uint256 indexed lockId, bool toPayee, uint256 bond);

    constructor(IEAS e, Registry r, Bank b) RulingBase(e, r, b) {}

    function demandOf(bytes memory demand) public pure returns (Demand memory) {
        return abi.decode(demand, (Demand));
    }

    /// Disputes a pay-unless-disputed lock within its window, with the bond.
    function dispute(uint256 lockId, string calldata reason) external payable {
        (bytes32 payer, bytes32 from, , uint256 rem, uint64 refundAfter, address cond, bytes memory demand) = _bankLock(lockId);
        require(cond == address(this), "not a ruling lock");
        Demand memory d = abi.decode(demand, (Demand));
        require(d.windowEnds != 0, "no dispute window: the deciders decide at once");
        if (!d.anyoneDisputes) {
            (bool ok,,) = registry.canAct(payer, msg.sender);
            if (!ok) (ok,,) = registry.canAct(from, msg.sender);
            require(ok, "only the payer disputes");
        } else {
            require(d.disputeBond > 0, "anyone disputes only with a bond");
        }
        require(block.timestamp <= d.windowEnds, "the challenge window is over");
        require(disputes[lockId].at == 0, "already disputed");
        require(rem > 0, "nothing left in the lock");
        // The deciders get MIN_DECIDE after the hearing too (audit 6 RU-2).
        uint64 from_ = d.hearingUntil > block.timestamp ? d.hearingUntil : uint64(block.timestamp);
        require(refundAfter >= from_ + MIN_DECIDE, "under 12 h before the refund time (after the hearing): too late for the deciders");
        require(msg.value == d.disputeBond, "the dispute bond");
        disputes[lockId] = Dispute(uint64(block.timestamp), false, msg.sender, msg.value);
        emit Disputed(lockId, d.subject, msg.sender, msg.value, reason);
    }

    function _askOf(uint256 lockId) internal view override returns (Ask memory) {
        Demand memory d = abi.decode(_demandOf(lockId), (Demand));
        uint64 since = d.hearingUntil;
        if (d.windowEnds != 0) {
            uint64 at = disputes[lockId].at;
            require(at != 0, "not disputed: no rulings on it");
            if (at > since) since = at;
        }
        return _ask(lockId, d.deciders, d.k, d.schema, d.subject, d.kindHash, since);
    }

    /// The deciders' share for the payee in bps (reverts until they decided).
    function share(uint256 lockId) public view returns (uint256) {
        Demand memory d = abi.decode(_demandOf(lockId), (Demand));
        return aggregate(_decision(lockId, "not enough rulings from the deciders"), d.rule, d.quota);
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        require(msg.sender == address(bank), "the bank asks");
        Demand memory d = abi.decode(demand, (Demand));
        if (d.windowEnds != 0 && disputes[lockId].at == 0) {
            require(toPayee, "not disputed: no early refund");
            require(block.timestamp > d.windowEnds, "the challenge window is open: paid after it unless disputed");
            return total - paid - refunded;
        }
        (,, bool acc) = ILockParties(msg.sender).parties(lockId);
        return _move(toPayee, acc, share(lockId), total, paid, refunded);
    }

    /// Once the lock is settled (nothing left): the bond to the payee's owner
    /// in proportion to the decided share, the rest back to the disputer; all
    /// of it to the payee if the deciders never decided. Anyone calls it.
    function settleBond(uint256 lockId) external {
        Dispute storage x = disputes[lockId];
        require(x.at != 0 && !x.bondSettled, "no bond to settle");
        (, , bytes32 payee, uint256 total, uint256 paid, uint256 refunded, , , ,) = bank.locks(lockId);
        require(paid + refunded == total, "the lock isn't settled yet");
        x.bondSettled = true;
        bytes32 to = registry.ownerOf(payee);
        if (to == bytes32(0)) { // a split: its first member's owner
            (bytes32[] memory ms,) = bank.splitOf(payee);
            if (ms.length > 0) to = registry.ownerOf(ms[0]);
        }
        uint256 forPayee = decided[lockId] ? x.bond * share(lockId) / 10000 : x.bond;
        if (to == bytes32(0)) forPayee = 0;
        if (forPayee > 0) bank.deposit{value: forPayee}(to);
        if (x.bond > forPayee) {
            (bool ok,) = payable(x.by).call{value: x.bond - forPayee}("");
            require(ok, "send");
        }
        emit BondSettled(lockId, forPayee > 0, x.bond);
    }
}

/// k of n: a lock released by a panel, not one person. Arbitration panels, a
/// community treasury, a jury. Each panelist attests a ruling (score = the
/// payee's share in basis points) about the deal's subject and records it;
/// with k of them, the lower median share applies. A hearing comes first:
/// rulings made before hearingUntil (or the lock's opening) don't count, so
/// nobody decides before both sides could be heard (their evidence is
/// timestamped on chain, sealed to the panel).
/// RulingCondition with rule = LOWER_MEDIAN and no dispute window.
contract PanelCondition is RulingBase, ICondition {
    struct Demand {
        bytes32[] panel;
        uint8 k;
        bytes32 schema;
        bytes32 subject;
        bytes32 kindHash;
        uint64 hearingUntil; // rulings attested before this don't count
    }

    constructor(IEAS e, Registry r, Bank b) RulingBase(e, r, b) {}

    function _askOf(uint256 lockId) internal view override returns (Ask memory) {
        Demand memory d = abi.decode(_demandOf(lockId), (Demand));
        return _ask(lockId, d.panel, d.k, d.schema, d.subject, d.kindHash, d.hearingUntil); // since: also the opening (RU-5)
    }

    /// The panel's share for the payee (reverts until k panelists recorded).
    function share(uint256 lockId) public view returns (uint256) {
        return aggregate(_decision(lockId, "not enough of the panel"), LOWER_MEDIAN, 0);
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        require(msg.sender == address(bank), "the bank asks");
        (,, bool acc) = ILockParties(msg.sender).parties(lockId);
        require(toPayee || acc, "the payee hasn't accepted this lock's terms: no early refund by ruling");
        return _move(toPayee, acc, share(lockId), total, paid, refunded);
    }
}

/// Pay unless disputed (an optimistic lock: UMA-style assertions, the freight
/// "net 30 unless a claim", shop returns): the payee is paid once the
/// challenge window ends, unless the payer disputed within it. A dispute
/// hands the decision to the deciders named at opening: k of n (k = 1 is one
/// arbiter), the lower median share in basis points, as PanelCondition.
/// Rulings count only if made for this lock after the dispute and recorded,
/// and the parties' own deciders don't count (nobody judges their own case).
/// As for any lock, what is left at the refund time goes back to the payer,
/// so the payee claims between the window's end and the refund time (anyone
/// may claim for it), and the deciders rule before the refund time; a
/// dispute must leave them MIN_DECIDE. RulingCondition with rule =
/// LOWER_MEDIAN, the payer disputing, no bond.
///   demand       abi.encode(Demand)
///   fulfillment  ignored (window over, no dispute: the payee is paid; disputed: the recorded rulings)
contract OptimisticCondition is RulingBase, ICondition {
    mapping(uint256 => uint64) public disputedAt; // lock => when its payer disputed (0: not disputed)

    struct Demand {
        uint64 windowEnds;
        bytes32[] deciders;
        uint8 k;
        bytes32 schema;
        bytes32 subject;
        bytes32 kindHash;
    }

    event Disputed(uint256 indexed lockId, bytes32 indexed subject, address by, string reason);

    constructor(IEAS e, Registry r, Bank b) RulingBase(e, r, b) {}

    /// The payer (its owner, or the agent that opened the lock) disputes
    /// within the window: the deciders decide from now on.
    function dispute(uint256 lockId, string calldata reason) external {
        (bytes32 payer, bytes32 from, , uint256 rem, uint64 refundAfter, address cond, bytes memory demand) = _bankLock(lockId);
        require(cond == address(this), "not a pay-unless-disputed lock");
        (bool ok,,) = registry.canAct(payer, msg.sender);
        if (!ok) (ok,,) = registry.canAct(from, msg.sender);
        require(ok, "only the payer disputes");
        Demand memory d = abi.decode(demand, (Demand));
        require(block.timestamp <= d.windowEnds, "the challenge window is over");
        require(disputedAt[lockId] == 0, "already disputed");
        require(rem > 0, "nothing left in the lock");
        require(refundAfter >= block.timestamp + MIN_DECIDE, "under 12 h before the refund time: too late for the deciders");
        disputedAt[lockId] = uint64(block.timestamp);
        emit Disputed(lockId, d.subject, msg.sender, reason);
    }

    function _askOf(uint256 lockId) internal view override returns (Ask memory) {
        Demand memory d = abi.decode(_demandOf(lockId), (Demand));
        uint64 at = disputedAt[lockId];
        require(at != 0, "not disputed: no rulings on it");
        return _ask(lockId, d.deciders, d.k, d.schema, d.subject, d.kindHash, at);
    }

    /// The deciders' share for the payee in basis points: k rulings, lower median.
    function share(uint256 lockId) public view returns (uint256) {
        return aggregate(_decision(lockId, "not enough rulings from the deciders since the dispute"), LOWER_MEDIAN, 0);
    }

    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        require(msg.sender == address(bank), "the bank asks");
        Demand memory d = abi.decode(demand, (Demand));
        if (disputedAt[lockId] == 0) {
            require(toPayee, "not disputed: no early refund");
            require(block.timestamp > d.windowEnds, "the challenge window is open: paid after it unless disputed");
            return total - paid - refunded;
        }
        (,, bool acc) = ILockParties(msg.sender).parties(lockId);
        return _move(toPayee, acc, share(lockId), total, paid, refunded);
    }
}
