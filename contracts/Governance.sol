// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";
import {ERC20Permit} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Permit.sol";
import {ERC20Votes} from "@openzeppelin/contracts/token/ERC20/extensions/ERC20Votes.sol";
import {Nonces} from "@openzeppelin/contracts/utils/Nonces.sol";
import {Ownable} from "@openzeppelin/contracts/access/Ownable.sol";
import {Governor} from "@openzeppelin/contracts/governance/Governor.sol";
import {GovernorSettings} from "@openzeppelin/contracts/governance/extensions/GovernorSettings.sol";
import {GovernorCountingSimple} from "@openzeppelin/contracts/governance/extensions/GovernorCountingSimple.sol";
import {GovernorVotes} from "@openzeppelin/contracts/governance/extensions/GovernorVotes.sol";
import {GovernorVotesQuorumFraction} from "@openzeppelin/contracts/governance/extensions/GovernorVotesQuorumFraction.sol";
import {TimelockController} from "@openzeppelin/contracts/governance/TimelockController.sol";
import {IVotes} from "@openzeppelin/contracts/governance/utils/IVotes.sol";

/// The network coin, wrapped one-to-one to vote with (OpenZeppelin
/// ERC20Votes, timestamp clock). Votes count from a snapshot taken when a
/// proposal is made: buying, voting and selling within a vote doesn't work.
contract WrappedTern is ERC20, ERC20Permit, ERC20Votes {
    constructor() ERC20("Wrapped Tern", "wTERN") ERC20Permit("Wrapped Tern") {}

    function deposit() external payable {
        _mint(msg.sender, msg.value);
    }

    function withdraw(uint256 amount) external {
        _burn(msg.sender, amount);
        (bool ok,) = payable(msg.sender).call{value: amount}("");
        require(ok, "send");
    }

    function clock() public view override returns (uint48) {
        return uint48(block.timestamp);
    }

    // solhint-disable-next-line func-name-mixedcase
    function CLOCK_MODE() public pure override returns (string memory) {
        return "mode=timestamp";
    }

    function _update(address from, address to, uint256 value) internal override(ERC20, ERC20Votes) {
        super._update(from, to, value);
    }

    function nonces(address owner) public view override(ERC20Permit, Nonces) returns (uint256) {
        return super.nonces(owner);
    }
}

/// Which contract is current for each key ("Bank" => address), with every
/// version it ever had: its specification, its audit report and migration
/// notes. Owned by the timelock: only proposals passed by both houses change
/// it, after the exit window. Old versions are never touched (contracts are
/// immutable and keep working); clients read the history to tell their owner
/// "Bank v3 is current, you use v2" and never move funds by themselves.
contract Directory is Ownable {
    struct Version {
        address value;
        bytes32 spec;      // hash of the specification
        bytes32 audit;     // hash of the audit report (0: none, launch versions)
        uint64 at;         // when it became current
        string specURI;
        string auditURI;
        string notes;      // migration notes: how to move from the version before
    }

    mapping(string => address) public current;
    mapping(string => bytes32) public specOf;
    mapping(string => string) public specURIOf;
    mapping(string => Version[]) internal history;
    mapping(bytes32 => mapping(address => uint256)) internal numberOf; // keccak(key) => address => version number (1-based)
    string[] internal keyList;

    event Set(string key, address value, bytes32 spec, string specURI);
    event Published(string key, uint256 indexed version, address value, bytes32 spec, bytes32 audit, string auditURI, string notes);

    constructor(address initialOwner) Ownable(initialOwner) {}

    /// A version without an audit report (the launch set).
    function set(string calldata key, address value, bytes32 spec, string calldata specURI) external onlyOwner {
        _publish(key, Version(value, spec, 0, uint64(block.timestamp), specURI, "", ""));
    }

    /// A new current version, with what it was approved on.
    function publish(string calldata key, address value, bytes32 spec, string calldata specURI, bytes32 audit,
        string calldata auditURI, string calldata notes) external onlyOwner {
        // A superseded or current version can't be published again: a stale
        // upgrade executed late can't roll the key back (audit 4 GV-4).
        require(numberOf[keccak256(bytes(key))][value] == 0, "already a version of this key");
        _publish(key, Version(value, spec, audit, uint64(block.timestamp), specURI, auditURI, notes));
    }

    function _publish(string calldata key, Version memory v) internal {
        require(bytes(key).length > 0 && bytes(key).length <= 64, "key");
        Version[] storage h = history[key];
        if (h.length == 0) keyList.push(key);
        h.push(v);
        bytes32 k = keccak256(bytes(key));
        if (numberOf[k][v.value] == 0) numberOf[k][v.value] = h.length;
        current[key] = v.value;
        specOf[key] = v.spec;
        specURIOf[key] = v.specURI;
        emit Set(key, v.value, v.spec, v.specURI);
        emit Published(key, h.length, v.value, v.spec, v.audit, v.auditURI, v.notes);
    }

    function keys() external view returns (string[] memory) {
        return keyList;
    }

    function versionCount(string calldata key) external view returns (uint256) {
        return history[key].length;
    }

    /// Version n (1-based) of key.
    function version(string calldata key, uint256 n) external view returns (Version memory) {
        return history[key][n - 1];
    }

    function versions(string calldata key) external view returns (Version[] memory) {
        return history[key];
    }

    /// Which version of key an address was first published as (0: never).
    function versionOf(string calldata key, address value) external view returns (uint256) {
        return numberOf[keccak256(bytes(key))][value];
    }
}

contract TernTimelock is TimelockController {
    constructor(uint256 minDelay, address[] memory proposers, address[] memory executors, address admin)
        TimelockController(minDelay, proposers, executors, admin) {}
}

/// The coin house: an OpenZeppelin Governor counting wTERN votes from a
/// snapshot, with a quorum as a share of all wTERN (and a floor). It only
/// counts: Parliament opens a proposal here for each of its own and reads the
/// result; executing a proposal here does nothing. Its settings change only
/// through the timelock, that is, by both houses.
contract TernGovernor is Governor, GovernorSettings, GovernorCountingSimple, GovernorVotes, GovernorVotesQuorumFraction {
    address public immutable timelock;
    uint256 public quorumFloor; // wTERN: the quorum is never below this, however little is wrapped

    constructor(IVotes token, address timelock_, uint48 delay_, uint32 period_, uint256 quorumPercent, uint256 floor_)
        Governor("Tern coin house")
        GovernorSettings(delay_, period_, 0)
        GovernorVotes(token)
        GovernorVotesQuorumFraction(quorumPercent) {
        timelock = timelock_;
        quorumFloor = floor_;
    }

    function setQuorumFloor(uint256 floor_) external onlyGovernance {
        quorumFloor = floor_;
    }

    /// Only the timelock governs this contract's settings.
    function _checkGovernance() internal view override {
        if (msg.sender != timelock) revert GovernorOnlyExecutor(msg.sender);
    }

    /// Counting only: the act is Parliament's, through the timelock.
    function _executeOperations(uint256, address[] memory, uint256[] memory, bytes[] memory, bytes32) internal pure override {}

    function votingDelay() public view override(Governor, GovernorSettings) returns (uint256) {
        return super.votingDelay();
    }

    function votingPeriod() public view override(Governor, GovernorSettings) returns (uint256) {
        return super.votingPeriod();
    }

    function quorum(uint256 timepoint) public view override(Governor, GovernorVotesQuorumFraction) returns (uint256) {
        uint256 q = super.quorum(timepoint);
        return q > quorumFloor ? q : quorumFloor;
    }

    function proposalThreshold() public view override(Governor, GovernorSettings) returns (uint256) {
        return super.proposalThreshold();
    }
}
