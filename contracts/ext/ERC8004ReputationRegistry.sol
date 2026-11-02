// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

interface IIdentity8004 {
    function ownerOf(uint256 agentId) external view returns (address);
    function isAuthorizedOrOwner(address spender, uint256 agentId) external view returns (bool);
}

/// ERC-8004 Reputation Registry for devnets and tests: the calls and events of
/// the one deployed on Base Sepolia (0x8004B663…8713, getVersion 2.0.0):
/// feedback is a signed fixed-point value with tags, a URI and a hash; no
/// one gives feedback to an agent they own or operate.
contract ERC8004ReputationRegistry {
    struct Feedback {
        int128 value;
        uint8 valueDecimals;
        string tag1;
        string tag2;
        bool isRevoked;
    }

    IIdentity8004 public immutable identityRegistry;
    mapping(uint256 => mapping(address => Feedback[])) private _fb;

    event NewFeedback(uint256 indexed agentId, address indexed clientAddress, uint64 feedbackIndex, int128 value, uint8 valueDecimals,
        string indexed indexedTag1, string tag1, string tag2, string endpoint, string feedbackURI, bytes32 feedbackHash);
    event FeedbackRevoked(uint256 indexed agentId, address indexed clientAddress, uint64 indexed feedbackIndex);

    constructor(IIdentity8004 identity) {
        identityRegistry = identity;
    }

    function getIdentityRegistry() external view returns (address) {
        return address(identityRegistry);
    }

    function getVersion() external pure returns (string memory) {
        return "2.0.0";
    }

    function giveFeedback(uint256 agentId, int128 value, uint8 valueDecimals, string calldata tag1, string calldata tag2,
        string calldata endpoint, string calldata feedbackURI, bytes32 feedbackHash) external {
        require(identityRegistry.ownerOf(agentId) != address(0), "agent does not exist");
        require(!identityRegistry.isAuthorizedOrOwner(msg.sender, agentId), "Self-feedback not allowed");
        require(valueDecimals <= 18, "too many decimals");
        _fb[agentId][msg.sender].push(Feedback(value, valueDecimals, tag1, tag2, false));
        uint64 index = uint64(_fb[agentId][msg.sender].length); // 1-based
        emit NewFeedback(agentId, msg.sender, index, value, valueDecimals, tag1, tag1, tag2, endpoint, feedbackURI, feedbackHash);
    }

    function revokeFeedback(uint256 agentId, uint64 feedbackIndex) external {
        require(feedbackIndex > 0 && feedbackIndex <= _fb[agentId][msg.sender].length, "no such feedback");
        _fb[agentId][msg.sender][feedbackIndex - 1].isRevoked = true;
        emit FeedbackRevoked(agentId, msg.sender, feedbackIndex);
    }

    function getLastIndex(uint256 agentId, address clientAddress) external view returns (uint64) {
        return uint64(_fb[agentId][clientAddress].length);
    }

    function readFeedback(uint256 agentId, address clientAddress, uint64 feedbackIndex) external view
        returns (int128 value, uint8 valueDecimals, string memory tag1, string memory tag2, bool isRevoked) {
        Feedback storage f = _fb[agentId][clientAddress][feedbackIndex - 1];
        return (f.value, f.valueDecimals, f.tag1, f.tag2, f.isRevoked);
    }

    /// Count and mean of the clients' unrevoked feedback with these tags (empty: any), at 18 decimals.
    function getSummary(uint256 agentId, address[] calldata clientAddresses, string calldata tag1, string calldata tag2)
        external view returns (uint64 count, int128 summaryValue, uint8 summaryValueDecimals) {
        require(clientAddresses.length > 0, "clientAddresses required");
        int256 sum;
        for (uint256 i = 0; i < clientAddresses.length; i++) {
            Feedback[] storage fs = _fb[agentId][clientAddresses[i]];
            for (uint256 j = 0; j < fs.length; j++) {
                Feedback storage f = fs[j];
                if (f.isRevoked) continue;
                if (bytes(tag1).length > 0 && keccak256(bytes(tag1)) != keccak256(bytes(f.tag1))) continue;
                if (bytes(tag2).length > 0 && keccak256(bytes(tag2)) != keccak256(bytes(f.tag2))) continue;
                sum += int256(f.value) * int256(10 ** (18 - f.valueDecimals));
                count++;
            }
        }
        if (count > 0) summaryValue = int128(sum / int256(uint256(count)));
        summaryValueDecimals = 18;
    }
}
