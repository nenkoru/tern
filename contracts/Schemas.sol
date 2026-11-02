// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import {ISchemaRegistry, SchemaRecord} from "@ethereum-attestation-service/eas-contracts/contracts/ISchemaRegistry.sol";

/// Names for types: card kinds, intents, reactions, ranker inputs, claim
/// kinds. The type itself is an EAS schema (SchemaRegistry: its fields, a
/// resolver if any); this registry maps a (publisher, name) to its schema's
/// uid, with versions. No admin, and no global names (audit 6 CM-3,
/// PROOFREAD-6 #22): every Tern name has its own namespace, so nobody can
/// squat "card/offer" for everyone; two publishers may both publish it, and a
/// client resolves a name through the publishers its owner pins (types.json).
/// A publisher's later versions are its own. Nothing here is trusted by
/// itself: a client renders an unknown kind generically.
///
/// Naming: "category/name", lowercase [a-z0-9._-] with one or more "/", at
/// most 64 bytes. The categories clients read: card/ (a card kind: FEED.md),
/// intent/ (a message intent), reaction/, signal/ (a ranker input), claim/
/// (a claim kind). Others are free.
contract SchemaNames {
    ISchemaRegistry public immutable schemas;
    Registry public immutable registry;

    struct Version {
        bytes32 uid; // the EAS schema
        uint64 at;
        string uri; // its documentation: a template, a response map, a spec
    }

    struct Entry {
        bytes32 publisher;
        string name;
    }

    mapping(bytes32 => Version[]) internal versionsOf; // keccak(publisher, name) => versions, oldest first
    mapping(bytes32 => string[]) internal namesBy; // publisher => its names, in order
    Entry[] internal entries; // every (publisher, name), in order

    event Published(string name, uint256 indexed version, bytes32 indexed uid, bytes32 indexed publisher, string uri);

    constructor(ISchemaRegistry s, Registry r) {
        schemas = s;
        registry = r;
    }

    function keyOf(bytes32 publisher, string memory name) public pure returns (bytes32) {
        return keccak256(abi.encode(publisher, name));
    }

    function validName(string calldata name) public pure returns (bool) {
        bytes calldata b = bytes(name);
        if (b.length < 3 || b.length > 64 || b[0] == "/" || b[b.length - 1] == "/") return false;
        bool slash;
        for (uint256 i = 0; i < b.length; i++) {
            bytes1 c = b[i];
            if (c == "/") {
                if (b[i - 1] == "/") return false;
                slash = true;
            } else if (!((c >= "a" && c <= "z") || (c >= "0" && c <= "9") || c == "." || c == "_" || c == "-")) {
                return false;
            }
        }
        return slash;
    }

    /// Publishes `uid` as the next version of `name` in by's namespace (by: the publisher's Tern name).
    function publish(bytes32 by, string calldata name, bytes32 uid, string calldata uri) external returns (uint256 version) {
        (bool ok,,) = registry.canAct(by, msg.sender);
        require(ok, "not yours");
        require(validName(name), "name: category/name, [a-z0-9._-]");
        SchemaRecord memory r = schemas.getSchema(uid);
        require(r.uid == uid && uid != 0, "no such EAS schema");
        bytes32 k = keyOf(by, name);
        if (versionsOf[k].length == 0) {
            namesBy[by].push(name);
            entries.push(Entry(by, name));
        }
        versionsOf[k].push(Version(uid, uint64(block.timestamp), uri));
        version = versionsOf[k].length;
        emit Published(name, version, uid, by, uri);
    }

    /// A publisher's newest version of name: its schema uid and its number (0: none).
    function latest(bytes32 publisher, string calldata name) external view returns (bytes32 uid, uint256 version) {
        Version[] storage v = versionsOf[keyOf(publisher, name)];
        version = v.length;
        if (version > 0) uid = v[version - 1].uid;
    }

    /// Version n (1-based) of a publisher's name.
    function version(bytes32 publisher, string calldata name, uint256 n) external view returns (Version memory) {
        return versionsOf[keyOf(publisher, name)][n - 1];
    }

    function versionCount(bytes32 publisher, string calldata name) external view returns (uint256) {
        return versionsOf[keyOf(publisher, name)].length;
    }

    /// A publisher's names (a client reads its pinned publishers' names only).
    function countBy(bytes32 publisher) external view returns (uint256) {
        return namesBy[publisher].length;
    }

    function nameAtBy(bytes32 publisher, uint256 i) external view returns (string memory) {
        return namesBy[publisher][i];
    }

    /// Every (publisher, name), for browsing.
    function count() external view returns (uint256) {
        return entries.length;
    }

    function entryAt(uint256 i) external view returns (bytes32 publisher, string memory name) {
        Entry storage e = entries[i];
        return (e.publisher, e.name);
    }
}
