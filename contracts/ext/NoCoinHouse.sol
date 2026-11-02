// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// The coin house of a network without a coin (the public profile: no token
/// is issued or wrapped): it abstains. Parliament asks both houses; this one
/// lets every proposal through once its voting period is over and holds no
/// votes, so the citizens' house (one vote each, secret ballots) decides
/// alone, with the same exit window and supermajority rules. It answers
/// Parliament's ICoinHouse and IVoteWeight calls and nothing else.
contract NoCoinHouse {
    uint256 public immutable votingPeriod;
    uint256 public proposals;
    mapping(uint256 => uint256) public proposalDeadline;

    event Proposed(uint256 indexed id, address indexed by, string description);

    constructor(uint256 votingPeriod_) {
        votingPeriod = votingPeriod_;
    }

    function propose(address[] memory, uint256[] memory, bytes[] memory, string memory description) external returns (uint256 id) {
        id = ++proposals;
        proposalDeadline[id] = block.timestamp + votingPeriod;
        emit Proposed(id, msg.sender, description);
    }

    /// 1 Active while the period runs, then 4 Succeeded (OpenZeppelin Governor's numbering).
    function state(uint256 id) external view returns (uint8) {
        require(proposalDeadline[id] != 0, "no such proposal");
        return block.timestamp > proposalDeadline[id] ? 4 : 1;
    }

    /// No coins, no votes against: a supermajority rule over this house holds vacuously.
    function proposalVotes(uint256) external pure returns (uint256 against, uint256 forVotes, uint256 abstain) {
        return (0, 1, 0);
    }

    /// Nobody holds coin votes: only citizens propose.
    function getVotes(address) external pure returns (uint256) {
        return 0;
    }
}
