// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Poseidon2.sol";

interface IPoolVerifier {
    function verifyProof(bytes calldata proof, uint256[6] calldata input) external view;
}

interface IAssociationSets {
    function valid(uint256 id, uint256 root) external view returns (bool);
}

interface IPoolBank {
    function deposit(bytes32 owner) external payable;
}

interface IPoolAnnouncer {
    function announce(uint256 schemeId, address stealthAddress, bytes memory ephemeralPubKey, bytes memory metadata) external;
}

/// Association sets (Privacy Pools): anyone publishes, under a name it then
/// owns, the root of a set of deposit labels it considers clean, with the
/// labels themselves (calldata in the event) so clients can rebuild and check
/// the tree. No fixed authority: a withdrawer picks the set it proves
/// membership in, and readers decide whose sets they accept. A replaced root
/// stays valid for GRACE, so withdrawals in flight still land.
contract AssociationSets {
    uint256 internal constant P = 21888242871839275222246405745257275088548364400416034343698204186575808495617;
    uint64 public constant GRACE = 3600;

    struct Set {
        address publisher;
        uint256 root;
        uint64 updated;
        uint64 count; // labels in the set now
        string name;
    }

    mapping(uint256 => Set) public sets;
    mapping(uint256 => mapping(uint256 => uint64)) public retiredAt; // id => root => when it was replaced
    uint256[] public ids;

    /// reset: labels is the whole set; otherwise they are appended to it.
    event Published(uint256 indexed id, string name, address indexed publisher, uint256 root, uint64 count, bool reset,
        uint256[] labels, string uri);

    function idOf(string calldata name) public pure returns (uint256) {
        return uint256(keccak256(bytes(name)));
    }

    function publish(string calldata name, uint256 root, uint256[] calldata labels, bool reset, string calldata uri)
        external returns (uint256 id) {
        require(bytes(name).length > 0 && bytes(name).length <= 64, "a name of 1-64 bytes");
        require(root != 0 && root < P, "root");
        id = idOf(name);
        Set storage s = sets[id];
        if (s.publisher == address(0)) {
            s.publisher = msg.sender;
            s.name = name;
            ids.push(id);
            reset = true;
        } else {
            require(s.publisher == msg.sender, "another publisher's set");
        }
        if (s.root != 0 && s.root != root) retiredAt[id][s.root] = uint64(block.timestamp);
        retiredAt[id][root] = 0;
        s.root = root;
        s.updated = uint64(block.timestamp);
        s.count = reset ? uint64(labels.length) : s.count + uint64(labels.length);
        emit Published(id, name, msg.sender, root, s.count, reset, labels, uri);
    }

    function valid(uint256 id, uint256 root) external view returns (bool) {
        if (root == 0) return false;
        if (sets[id].root == root) return true;
        uint64 r = retiredAt[id][root];
        return r != 0 && block.timestamp < uint256(r) + GRACE;
    }

    function count() external view returns (uint256) {
        return ids.length;
    }
}

/// A shielded pool of the network coin, after Privacy Pools (Buterin, Illum,
/// Nadler, Schär, Soleimani, 2023) as built by 0xbow (privacy-pools-core):
///   deposit:  note = H(value, label, H(nullifier, secret)); label =
///             keccak(scope, nonce) mod p, assigned here; the note joins the
///             state tree (Poseidon2, depth 20).
///   withdraw: a Groth16 proof that some note in a recent state root is
///             spent (its nullifier hash revealed once), that its label is in
///             an association set the withdrawer chose, and that the change
///             H(value - withdrawn, label, H(nullifier', secret')) is the new
///             note. Any amount up to the note's; the label travels with the
///             change, so the association set still covers it.
/// Withdrawals go to an address (optionally announced as an ERC-5564 stealth
/// payment) or to a name's Bank balance. Anyone may submit one: a relayer
/// pays the gas and takes `fee` from the amount; the proof's context binds
/// recipient, relayer and fee, so nobody can redirect it.
contract PrivacyPool {
    uint256 internal constant P = 21888242871839275222246405745257275088548364400416034343698204186575808495617;
    uint256 public constant DEPTH = 20;
    uint256 public constant HISTORY = 64; // recent state roots a proof may use (0xbow: 64)
    uint256 public constant ROOT_GRACE = 3600; // and any root made this recently (audit 5 PV-7: dust deposits can't expire it)
    address internal constant NATIVE = 0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE;

    IPoolVerifier public immutable verifier;
    IAssociationSets public immutable asps;
    IPoolBank public immutable bank;
    IPoolAnnouncer public immutable announcer;
    uint256 public immutable SCOPE;
    address public immutable hasher; // Poseidon2Hasher

    uint256 public nonce;
    uint256 public size; // notes in the tree
    uint256 public root;
    uint256 public roots; // roots so far
    uint256[DEPTH] internal filled;
    mapping(uint256 => uint256) public rootSeq; // root => its number (1-based) | when it was made << 128
    mapping(uint256 => bool) public spent; // nullifier hash => spent
    mapping(uint256 => bool) public usedPrecommitment;
    mapping(uint256 => address) public depositorOf; // label => who deposited (public anyway)

    struct Withdrawal {
        address recipient; // 0 when paying a name
        bytes32 name; // a Registry name: paid into its Bank balance
        address relayer; // takes the fee
        uint256 fee;
        uint256 aspId; // the association set chosen
        bytes ephemeralPubKey; // 33 bytes: announce the recipient as an ERC-5564 stealth address
        bytes1 viewTag;
    }

    event Deposited(address indexed depositor, uint256 commitment, uint256 label, uint256 value, uint256 precommitment,
        uint256 index, uint256 root);
    event Withdrawn(uint256 indexed nullifierHash, uint256 value, uint256 newCommitment, uint256 index, uint256 root,
        address recipient, bytes32 name, address relayer, uint256 fee, uint256 aspId, uint256 aspRoot);

    constructor(IPoolVerifier v, IAssociationSets a, IPoolBank b, IPoolAnnouncer ann, address h) {
        verifier = v;
        hasher = h;
        asps = a;
        bank = b;
        announcer = ann;
        SCOPE = uint256(keccak256(abi.encodePacked(address(this), block.chainid, NATIVE))) % P;
        root = Poseidon2.zero(DEPTH);
        _root(root);
    }

    function deposit(uint256 precommitment) external payable returns (uint256 commitment, uint256 label) {
        require(msg.value > 0 && msg.value < 2 ** 128, "amount");
        require(precommitment != 0 && precommitment < P && !usedPrecommitment[precommitment], "precommitment");
        usedPrecommitment[precommitment] = true;
        label = uint256(keccak256(abi.encodePacked(SCOPE, ++nonce))) % P;
        depositorOf[label] = msg.sender;
        commitment = Poseidon2.hash3(hasher, msg.value, label, precommitment);
        uint256 index = _insert(commitment);
        emit Deposited(msg.sender, commitment, label, msg.value, precommitment, index, root);
    }

    function context(Withdrawal calldata w) public view returns (uint256) {
        return uint256(keccak256(abi.encode(w, SCOPE))) % P;
    }

    function knownRoot(uint256 r) public view returns (bool) {
        uint256 s = rootSeq[r];
        return s != 0 && (uint128(s) + HISTORY > roots || block.timestamp < (s >> 128) + ROOT_GRACE);
    }

    function withdraw(Withdrawal calldata w, uint256 value, uint256 stateRoot, uint256 aspRoot, uint256 nullifierHash,
        uint256 newCommitment, bytes calldata proof) external {
        require(value > 0 && w.fee <= value, "amount or fee");
        require(!spent[nullifierHash], "spent");
        require(knownRoot(stateRoot), "unknown state root");
        require(asps.valid(w.aspId, aspRoot), "not a current root of that association set");
        require((w.name == 0) != (w.recipient == address(0)), "an address or a name");
        require(w.fee == 0 || w.relayer != address(0), "relayer");
        verifier.verifyProof(proof, [newCommitment, nullifierHash, value, stateRoot, aspRoot, context(w)]);
        spent[nullifierHash] = true;
        uint256 index = _insert(newCommitment);
        emit Withdrawn(nullifierHash, value, newCommitment, index, root, w.recipient, w.name, w.relayer, w.fee, w.aspId, aspRoot);
        uint256 net = value - w.fee;
        if (net > 0) {
            if (w.name != 0) {
                bank.deposit{value: net}(w.name);
            } else {
                (bool ok,) = w.recipient.call{value: net}("");
                require(ok, "send");
                if (w.ephemeralPubKey.length == 33) {
                    announcer.announce(1, w.recipient, w.ephemeralPubKey,
                        abi.encodePacked(w.viewTag, bytes4(0xeeeeeeee), NATIVE, net, bytes32(0)));
                }
            }
        }
        if (w.fee > 0) {
            (bool ok,) = w.relayer.call{value: w.fee}("");
            require(ok, "fee");
        }
    }

    function _insert(uint256 leaf) internal returns (uint256 index) {
        require(leaf < P, "not a field element");
        index = size;
        require(index < 2 ** DEPTH, "the pool is full");
        size = index + 1;
        uint256 cur = leaf;
        uint256 i = index;
        for (uint256 d = 0; d < DEPTH; d++) {
            if (i & 1 == 0) {
                filled[d] = cur;
                cur = Poseidon2.compress(hasher, cur, Poseidon2.zero(d));
            } else {
                cur = Poseidon2.compress(hasher, filled[d], cur);
            }
            i >>= 1;
        }
        root = cur;
        _root(cur);
    }

    function _root(uint256 r) internal {
        rootSeq[r] = ++roots | (block.timestamp << 128);
    }
}
