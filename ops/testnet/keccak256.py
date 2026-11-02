#!/usr/bin/env python3
"""keccak256 of a file, as `ternchain validator genesis` prints it ("keccak").

    python3 -I ops/testnet/keccak256.py genesis.json   ->  0x…

Plain Python, no packages: Keccak-256 (Ethereum's hash, not SHA3-256).
Slow on big files, fine for a genesis.json.
"""
import sys

RC = [
    0x0000000000000001, 0x0000000000008082, 0x800000000000808A, 0x8000000080008000,
    0x000000000000808B, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
    0x000000000000008A, 0x0000000000000088, 0x0000000080008009, 0x000000008000000A,
    0x000000008000808B, 0x800000000000008B, 0x8000000000008089, 0x8000000000008003,
    0x8000000000008002, 0x8000000000000080, 0x000000000000800A, 0x800000008000000A,
    0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
]
ROT = [
    [0, 36, 3, 41, 18], [1, 44, 10, 45, 2], [62, 6, 43, 15, 61],
    [28, 55, 25, 21, 56], [27, 20, 39, 8, 14],
]
M = (1 << 64) - 1


def rol(v, n):
    return ((v << n) | (v >> (64 - n))) & M if n else v


def f1600(a):
    for rc in RC:
        c = [a[x][0] ^ a[x][1] ^ a[x][2] ^ a[x][3] ^ a[x][4] for x in range(5)]
        d = [c[(x - 1) % 5] ^ rol(c[(x + 1) % 5], 1) for x in range(5)]
        a = [[a[x][y] ^ d[x] for y in range(5)] for x in range(5)]
        b = [[0] * 5 for _ in range(5)]
        for x in range(5):
            for y in range(5):
                b[y][(2 * x + 3 * y) % 5] = rol(a[x][y], ROT[x][y])
        a = [[b[x][y] ^ ((~b[(x + 1) % 5][y]) & b[(x + 2) % 5][y]) for y in range(5)] for x in range(5)]
        a[0][0] ^= rc
    return a


def keccak256(data: bytes) -> bytes:
    rate = 136
    msg = bytearray(data)
    msg.append(0x01)  # Keccak padding (SHA3 would use 0x06)
    while len(msg) % rate:
        msg.append(0)
    msg[-1] |= 0x80
    a = [[0] * 5 for _ in range(5)]
    for off in range(0, len(msg), rate):
        block = msg[off:off + rate]
        for i in range(rate // 8):
            x, y = i % 5, i // 5
            a[x][y] ^= int.from_bytes(block[8 * i:8 * i + 8], "little")
        a = f1600(a)
    out = b"".join(a[i % 5][i // 5].to_bytes(8, "little") for i in range(4))
    return out


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit("usage: keccak256.py FILE")
    with open(sys.argv[1], "rb") as f:
        print("0x" + keccak256(f.read()).hex())
