// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import {IERC20} from "@openzeppelin/contracts/token/ERC20/IERC20.sol";

interface IJobsBank {
    function locks(uint256 id) external view returns (bytes32 payer, bytes32 from, bytes32 payee, uint256 total, uint256 paid,
        uint256 refunded, uint64 refundAfter, address condition, bytes memory demand, address token);
    function claim(uint256 id, bytes calldata fulfillment) external;
    function refundEarly(uint256 id, bytes calldata fulfillment) external;
    function hold(uint256 id) external;
}

interface IJobsRegistry {
    function canAct(bytes32 id, address who) external view returns (bool ok, bytes32 owner, bool isAgent);
}

/// ERC-8183 (Agentic Commerce): a job with a client, a provider and an
/// evaluator, escrowed in one ERC-20 (the network's USDC):
///   Open -> Funded -> Submitted -> Completed | Rejected | Expired.
///
/// Two ways to fund a job, one job interface:
/// - plainly, as the standard says: fund(jobId, "") pulls the budget from
///   the client (any ERC-8183 client, ternchain or not);
/// - from a ternchain Bank lock: fund(jobId, abi.encode(lockId)), where the
///   client's name (or its agent, within the limit its owner set) locked the
///   budget for the provider's name with this contract as the lock's
///   condition. The money stays in the Bank, held: only the job's outcome
///   moves it (Completed pays the provider, Rejected and Expired refund).
///   Ratings count it as a paid deal like any other lock.
/// Hooks (IACPHook) are not supported: createJob takes hook = 0.
contract TernJobs {
    enum JobStatus {
        Open,
        Funded,
        Submitted,
        Completed,
        Rejected,
        Expired
    }

    struct Job {
        uint256 id;
        address client;
        address provider;
        address evaluator;
        string description;
        uint256 budget;
        uint256 expiredAt;
        JobStatus status;
        address hook;
    }

    IERC20 public immutable paymentToken;
    IJobsBank public immutable bank;
    IJobsRegistry public immutable registry;

    Job[] private _jobs;
    mapping(uint256 => uint256) public lockOf; // job => Bank lock id + 1 (0: funded plainly, or not yet)
    mapping(uint256 => bytes32) public deliverableOf;

    event JobCreated(uint256 indexed jobId, address indexed client, address indexed provider, address evaluator, uint256 expiredAt, address hook);
    event ProviderSet(uint256 indexed jobId, address indexed provider);
    event BudgetSet(uint256 indexed jobId, uint256 amount);
    event JobFunded(uint256 indexed jobId, address indexed client, uint256 amount);
    event JobSubmitted(uint256 indexed jobId, address indexed provider, bytes32 deliverable);
    event JobCompleted(uint256 indexed jobId, address indexed evaluator, bytes32 reason);
    event JobRejected(uint256 indexed jobId, address indexed rejector, bytes32 reason);
    event JobExpired(uint256 indexed jobId);
    event PaymentReleased(uint256 indexed jobId, address indexed provider, uint256 amount);
    event Refunded(uint256 indexed jobId, address indexed client, uint256 amount);
    event FundedFromLock(uint256 indexed jobId, uint256 indexed lockId);

    constructor(IERC20 token, IJobsBank bank_, IJobsRegistry registry_) {
        paymentToken = token;
        bank = bank_;
        registry = registry_;
    }

    function jobCount() external view returns (uint256) {
        return _jobs.length;
    }

    function getJob(uint256 jobId) external view returns (Job memory) {
        return _jobs[jobId];
    }

    function createJob(address provider, address evaluator, uint256 expiredAt, string calldata description, address hook)
        external returns (uint256 jobId) {
        require(evaluator != address(0), "evaluator required");
        require(expiredAt > block.timestamp + 5 minutes, "expiry too soon");
        require(hook == address(0), "hooks not supported");
        jobId = _jobs.length;
        _jobs.push(Job(jobId, msg.sender, provider, evaluator, description, 0, expiredAt, JobStatus.Open, address(0)));
        emit JobCreated(jobId, msg.sender, provider, evaluator, expiredAt, address(0));
    }

    /// A job made without a provider (a tender): the client picks one.
    function setProvider(uint256 jobId, address provider_) external {
        Job storage j = _jobs[jobId];
        require(msg.sender == j.client, "client only");
        require(j.status == JobStatus.Open, "not open");
        require(j.provider == address(0) && provider_ != address(0), "provider set");
        j.provider = provider_;
        emit ProviderSet(jobId, provider_);
    }

    /// The price, proposed by the provider or set by the client, while open.
    function setBudget(uint256 jobId, uint256 amount, bytes calldata) external {
        Job storage j = _jobs[jobId];
        require(msg.sender == j.client || msg.sender == j.provider, "client or provider only");
        require(j.status == JobStatus.Open, "not open");
        j.budget = amount;
        emit BudgetSet(jobId, amount);
    }

    function fund(uint256 jobId, bytes calldata optParams) external {
        Job storage j = _jobs[jobId];
        require(msg.sender == j.client, "client only");
        require(j.status == JobStatus.Open, "not open");
        require(j.provider != address(0), "no provider");
        require(j.budget > 0, "no budget");
        require(block.timestamp < j.expiredAt, "expired");
        if (optParams.length == 32) {
            uint256 lockId = abi.decode(optParams, (uint256));
            (, bytes32 from, bytes32 payee, uint256 total, uint256 paid, uint256 refunded, uint64 refundAfter, address condition,
                bytes memory demand, address token) = bank.locks(lockId);
            require(condition == address(this) && keccak256(demand) == keccak256(abi.encode(jobId)), "not this job's lock");
            require(token == address(paymentToken) && total == j.budget && paid == 0 && refunded == 0, "the lock must hold the budget");
            require(refundAfter >= j.expiredAt, "the lock must last until the job expires");
            (bool byClient, , ) = registry.canAct(from, msg.sender);
            (bool toProvider, , ) = registry.canAct(payee, j.provider);
            require(byClient && toProvider, "the lock must be from the client's name to the provider's");
            require(lockOf[jobId] == 0, "funded");
            lockOf[jobId] = lockId + 1;
            bank.hold(lockId); // only the job's outcome moves it now
            emit FundedFromLock(jobId, lockId);
        } else {
            require(paymentToken.transferFrom(msg.sender, address(this), j.budget), "transfer");
        }
        j.status = JobStatus.Funded;
        emit JobFunded(jobId, msg.sender, j.budget);
    }

    function submit(uint256 jobId, bytes32 deliverable, bytes calldata) external {
        Job storage j = _jobs[jobId];
        require(msg.sender == j.provider, "provider only");
        require(j.status == JobStatus.Funded, "not funded");
        j.status = JobStatus.Submitted;
        deliverableOf[jobId] = deliverable;
        emit JobSubmitted(jobId, msg.sender, deliverable);
    }

    function complete(uint256 jobId, bytes32 reason, bytes calldata) external {
        Job storage j = _jobs[jobId];
        require(msg.sender == j.evaluator, "evaluator only");
        require(j.status == JobStatus.Submitted, "not submitted");
        j.status = JobStatus.Completed;
        emit JobCompleted(jobId, msg.sender, reason);
        if (lockOf[jobId] != 0) bank.claim(lockOf[jobId] - 1, "");
        else require(paymentToken.transfer(j.provider, j.budget), "transfer");
        emit PaymentReleased(jobId, j.provider, j.budget);
    }

    /// The client while it is open; the evaluator once funded.
    function reject(uint256 jobId, bytes32 reason, bytes calldata) external {
        Job storage j = _jobs[jobId];
        if (j.status == JobStatus.Open) require(msg.sender == j.client, "client only while open");
        else require((j.status == JobStatus.Funded || j.status == JobStatus.Submitted) && msg.sender == j.evaluator, "evaluator only");
        bool funded = j.status != JobStatus.Open;
        j.status = JobStatus.Rejected;
        emit JobRejected(jobId, msg.sender, reason);
        if (funded) _refund(jobId, j);
    }

    /// After the expiry anyone may send a funded job's money back to its client.
    function claimRefund(uint256 jobId) external {
        Job storage j = _jobs[jobId];
        require(j.status == JobStatus.Funded || j.status == JobStatus.Submitted, "not refundable");
        require(block.timestamp >= j.expiredAt, "not expired");
        j.status = JobStatus.Expired;
        emit JobExpired(jobId);
        _refund(jobId, j);
    }

    function _refund(uint256 jobId, Job storage j) internal {
        if (lockOf[jobId] != 0) bank.refundEarly(lockOf[jobId] - 1, "");
        else require(paymentToken.transfer(j.client, j.budget), "transfer");
        emit Refunded(jobId, j.client, j.budget);
    }

    /// The Bank's condition (ICondition): a job's lock pays out when the job
    /// is completed and goes back when it is rejected or expired.
    function check(uint256 lockId, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total, uint256 paid,
        uint256 refunded) external view returns (uint256) {
        uint256 jobId = abi.decode(demand, (uint256));
        if (jobId >= _jobs.length || lockOf[jobId] != lockId + 1) return 0;
        JobStatus s = _jobs[jobId].status;
        bool due = toPayee ? s == JobStatus.Completed : (s == JobStatus.Rejected || s == JobStatus.Expired);
        return due ? total - paid - refunded : 0;
    }
}
