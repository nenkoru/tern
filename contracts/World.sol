// SPDX-License-Identifier: Apache-2.0
pragma solidity ^0.8.24;

import "./Registry.sol";
import "./Bank.sol";
import "./Commitments.sol";
import {ERC1155} from "@openzeppelin/contracts/token/ERC1155/ERC1155.sol";
import {ERC20} from "@openzeppelin/contracts/token/ERC20/ERC20.sol";

/// Primitives for worlds people and agents build: fair randomness, a clock
/// for turns and transferable items (decisions by a panel: Ruling.sol).

/// Fair randomness from many parties. A round under a topic takes every
/// commitment made under that topic before commitBy; the seed mixes what each
/// revealed by revealBy with the block's randomness. Nobody can steer it
/// alone: a late entry doesn't count, and refusing to reveal only drops your
/// entry (put a reveal bond on it, RevealCondition, to make that costly).
contract Random {
    Commitments public immutable commitments;

    struct Round {
        uint64 commitBy;
        uint64 revealBy;
        bytes32 seed;
        uint32 entries;
        bool drawn;
    }

    mapping(bytes32 => Round) public rounds; // by topic

    event Opened(bytes32 indexed topic, uint64 commitBy, uint64 revealBy);
    event Drawn(bytes32 indexed topic, bytes32 seed, uint32 entries);

    constructor(Commitments c) {
        commitments = c;
    }

    function open(bytes32 topic, uint64 commitBy, uint64 revealBy) external {
        require(rounds[topic].commitBy == 0, "round exists");
        require(commitBy > block.timestamp && revealBy > commitBy, "times");
        rounds[topic] = Round(commitBy, revealBy, bytes32(0), 0, false);
        emit Opened(topic, commitBy, revealBy);
    }

    function draw(bytes32 topic) external returns (bytes32 seed) {
        Round storage r = rounds[topic];
        require(r.commitBy != 0 && !r.drawn, "no open round");
        require(block.timestamp > r.commitBy, "still taking entries");
        uint256[] memory ids = commitments.idsOf(topic);
        bytes32 acc = topic;
        uint32 n;
        bool all = true;
        for (uint256 i = 0; i < ids.length; i++) {
            (, , , , bool revealed, uint64 at, uint64 revealedAt, bytes32 opened) = commitments.all(ids[i]);
            if (at > r.commitBy) continue; // late entries don't count
            if (revealed && revealedAt <= r.revealBy) {
                acc = keccak256(abi.encode(acc, opened));
                n++;
            } else {
                all = false;
            }
        }
        require(all || block.timestamp > r.revealBy, "waiting for reveals");
        require(n > 0, "no revealed entries");
        seed = keccak256(abi.encode(acc, block.prevrandao));
        r.seed = seed;
        r.entries = n;
        r.drawn = true;
        emit Drawn(topic, seed, n);
    }

    /// A die with `sides` faces, the index-th roll from the seed.
    function roll(bytes32 topic, uint256 sides, uint256 index) external view returns (uint256) {
        require(rounds[topic].drawn && sides > 0, "not drawn");
        return uint256(keccak256(abi.encode(rounds[topic].seed, index))) % sides;
    }
}

/// A clock for turns: chess, poker rounds, negotiations with deadlines.
/// Players move in order; each has perMove seconds. Anyone can end a game
/// whose player ran out of time. As a lock condition (demand: game id and a
/// player), a stake goes to the payee if that player timed out or resigned,
/// and back to the payer when the game ends otherwise.
contract Clock is ICondition {
    Registry public immutable registry;

    struct Game {
        bytes32[] players;
        bytes32 referee; // ends the game normally; zero = all players agree
        uint64 perMove;
        uint64 deadline;
        uint32 turn;
        uint8 state; // 1 playing, 2 over
        bytes32 loser; // timed out or resigned; zero = a normal end
        uint256 agreed; // bitmask of players who agreed to end
        bytes32 log; // hash chain of moves
    }

    Game[] internal games;

    event Started(uint256 indexed id, bytes32[] players, bytes32 referee, uint64 perMove, uint64 deadline);
    event Moved(uint256 indexed id, uint32 turn, bytes32 indexed by, bytes data, uint64 deadline);
    event Ended(uint256 indexed id, bytes32 loser, string how);

    constructor(Registry r) {
        registry = r;
    }

    function start(bytes32[] calldata players, bytes32 referee, uint64 perMove) external returns (uint256 id) {
        require(players.length >= 2 && players.length <= 32 && perMove >= 10, "players or time");
        id = games.length;
        Game storage g = games.push();
        g.players = players;
        g.referee = referee;
        g.perMove = perMove;
        g.deadline = uint64(block.timestamp) + perMove;
        g.state = 1;
        emit Started(id, players, referee, perMove, g.deadline);
    }

    function toMove(uint256 id) public view returns (bytes32) {
        Game storage g = games[id];
        return g.players[g.turn % g.players.length];
    }

    function move(uint256 id, bytes calldata data) external {
        Game storage g = games[id];
        require(g.state == 1, "not playing");
        require(block.timestamp <= g.deadline, "out of time");
        bytes32 by = toMove(id);
        (bool ok,,) = registry.canAct(by, msg.sender);
        require(ok, "not your move");
        g.log = keccak256(abi.encode(g.log, data));
        g.turn++;
        g.deadline = uint64(block.timestamp) + g.perMove;
        emit Moved(id, g.turn, by, data, g.deadline);
    }

    function timeout(uint256 id) external {
        Game storage g = games[id];
        require(g.state == 1 && block.timestamp > g.deadline, "not timed out");
        g.state = 2;
        g.loser = toMove(id);
        emit Ended(id, g.loser, "timed out");
    }

    function resign(uint256 id, bytes32 player) external {
        Game storage g = games[id];
        require(g.state == 1, "not playing");
        (bool ok,,) = registry.canAct(player, msg.sender);
        require(ok && indexOf(g, player) < 32, "not a player");
        g.state = 2;
        g.loser = player;
        emit Ended(id, player, "resigned");
    }

    /// A normal end: by the referee, or once every player agrees.
    function finish(uint256 id, bytes32 by) external {
        Game storage g = games[id];
        require(g.state == 1, "not playing");
        (bool ok,,) = registry.canAct(by, msg.sender);
        require(ok, "not yours");
        if (g.referee != bytes32(0)) {
            require(by == g.referee, "referee only");
        } else {
            uint256 i = indexOf(g, by);
            require(i < 32, "not a player");
            g.agreed |= 1 << i;
            if (g.agreed != (1 << g.players.length) - 1) return;
        }
        g.state = 2;
        emit Ended(id, bytes32(0), "finished");
    }

    function indexOf(Game storage g, bytes32 p) internal view returns (uint256) {
        for (uint256 i = 0; i < g.players.length; i++) if (g.players[i] == p) return i;
        return 32;
    }

    function info(uint256 id) external view returns (bytes32[] memory players, bytes32 next, uint64 deadline, uint32 turn,
        uint8 state, bytes32 loser) {
        Game storage g = games[id];
        return (g.players, g.state == 1 ? toMove(id) : bytes32(0), g.deadline, g.turn, g.state, g.loser);
    }

    function count() external view returns (uint256) {
        return games.length;
    }

    function check(uint256, bool toPayee, address, bytes calldata demand, bytes calldata, uint256 total,
        uint256 paid, uint256 refunded) external view returns (uint256) {
        (uint256 id, bytes32 player) = abi.decode(demand, (uint256, bytes32));
        Game storage g = games[id];
        if (g.state != 2) return 0;
        uint256 rem = total - paid - refunded;
        bool lost = g.loser == player;
        return toPayee == lost ? rem : 0;
    }
}

/// Things that can be owned and handed on: in-game items, land, tickets,
/// badges. Plain ERC-1155, so wallets and markets understand it. Anyone
/// creates a class (one-of-a-kind or many); a soulbound class can't be
/// transferred (a diploma, a membership). Selling is not this contract's
/// business: the Bank escrows ERC-1155 and ERC-721 (Bank.lockItem), and
/// SwapCondition swaps an item lock for a coin or token lock atomically. The
/// offer book that once lived here is examples/ItemOffers.sol.
contract Items is ERC1155 {
    Registry public immutable registry;

    struct Class {
        bytes32 creator;
        uint256 maxSupply; // 0 = no cap
        uint256 minted;
        bool soulbound;
        string uri;
    }

    Class[] public classes;

    event ClassCreated(uint256 indexed id, bytes32 indexed creator, uint256 maxSupply, bool soulbound, string uri);

    constructor(Registry r) ERC1155("") {
        registry = r;
    }

    function create(bytes32 creator, string calldata itemUri, uint256 maxSupply, bool soulbound) external returns (uint256 id) {
        (bool ok,,) = registry.canAct(creator, msg.sender);
        require(ok, "not yours");
        id = classes.length;
        classes.push(Class(creator, maxSupply, 0, soulbound, itemUri));
        emit ClassCreated(id, creator, maxSupply, soulbound, itemUri);
    }

    function mint(uint256 id, address to, uint256 amount) external {
        Class storage c = classes[id];
        (bool ok,,) = registry.canAct(c.creator, msg.sender);
        require(ok, "creator only");
        require(c.maxSupply == 0 || c.minted + amount <= c.maxSupply, "supply cap");
        c.minted += amount;
        _mint(to, id, amount, "");
    }

    function uri(uint256 id) public view override returns (string memory) {
        return classes[id].uri;
    }

    function _update(address from, address to, uint256[] memory ids, uint256[] memory values) internal override {
        if (from != address(0) && to != address(0)) {
            for (uint256 i = 0; i < ids.length; i++) require(!classes[ids[i]].soulbound, "soulbound: can't be transferred");
        }
        super._update(from, to, ids, values);
    }

    function classCount() external view returns (uint256) {
        return classes.length;
    }
}

/// A stand-in for a stablecoin on devnets (deploy puts it only on the devnet chain
/// ids, 1337 and 31337, or with the devnet flag). Anyone can mint: test money only.
contract TestUSD is ERC20 {
    constructor() ERC20("Test USD", "tUSD") {}

    function mint(address to, uint256 amount) external {
        _mint(to, amount);
    }
}
