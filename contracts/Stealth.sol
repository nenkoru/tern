// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

/// ERC-5564 Announcer (the standard singleton's interface and event).
contract ERC5564Announcer {
    event Announcement(uint256 indexed schemeId, address indexed stealthAddress, address indexed caller,
        bytes ephemeralPubKey, bytes metadata);

    function announce(uint256 schemeId, address stealthAddress, bytes memory ephemeralPubKey, bytes memory metadata)
        external {
        emit Announcement(schemeId, stealthAddress, msg.sender, ephemeralPubKey, metadata);
    }
}

/// ERC-6538 Stealth Meta-Address Registry (minimal: direct registration).
contract ERC6538Registry {
    mapping(address registrant => mapping(uint256 schemeId => bytes)) public stealthMetaAddressOf;

    event StealthMetaAddressSet(address indexed registrant, uint256 indexed schemeId, bytes stealthMetaAddress);

    function registerKeys(uint256 schemeId, bytes calldata stealthMetaAddress) external {
        stealthMetaAddressOf[msg.sender][schemeId] = stealthMetaAddress;
        emit StealthMetaAddressSet(msg.sender, schemeId, stealthMetaAddress);
    }
}

/// Pays native coins to a stealth address and announces it in one step,
/// with the ERC-5564 recommended metadata for native transfers:
///   viewTag(1) | 0xeeeeeeee(4) | 0xEeee…EEeE(20) | amount(32) | memo(32)
/// (memo is ours: an envelope hash, for private postage).
contract StealthPay {
    ERC5564Announcer public immutable announcer;
    address constant NATIVE = 0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE;

    constructor(ERC5564Announcer a) {
        announcer = a;
    }

    function send(address payable stealth, bytes calldata ephemeralPubKey, bytes1 viewTag, bytes32 memo) external payable {
        require(ephemeralPubKey.length == 33, "compressed ephemeral key");
        (bool ok,) = stealth.call{value: msg.value}("");
        require(ok, "send");
        bytes memory metadata = abi.encodePacked(viewTag, bytes4(0xeeeeeeee), NATIVE, msg.value, memo);
        announcer.announce(1, stealth, ephemeralPubKey, metadata);
    }
}
