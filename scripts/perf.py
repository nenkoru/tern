#!/usr/bin/env python3
"""Latency on a fresh local devnet with a data node (docs/PERF.md).

  scripts/perf.py [--bin ./ternchain] [--runs 10] [--channel]

Starts a devnet (1 s blocks, in memory) and a free data node, makes alice,
bob, carol and alice's agent ada (tainted by carol's mail, free_pay 0.01),
keeps `watch` running for bob, and times, from the moment the command starts:
  mail     alice `send` -> bob's watch prints the mail
  approve  alice `owner approve ada bob 0.5` -> ada's `bank pay bob 0.5` succeeds
  pay      alice `bank pay bob 0.001` -> bob's watch prints `paid`
  channel  (--channel) alice `channel pay bob 0.001` -> bob's watch prints `channel-paid`
Prints p50/p95 per metric (seconds) as JSON.
"""
import argparse, json, os, queue, random, shutil, subprocess, sys, tempfile, threading, time

ap = argparse.ArgumentParser()
ap.add_argument("--bin", default="./ternchain")
ap.add_argument("--runs", type=int, default=10)
ap.add_argument("--channel", action="store_true")
ap.add_argument("--keep", action="store_true")
args = ap.parse_args()

T = os.path.abspath(args.bin)
work = tempfile.mkdtemp(prefix="ternperf-")
port = 18000 + random.randint(0, 900) * 10
net = os.path.join(work, "net.json")
relay = f"http://127.0.0.1:{port + 2}"
env = dict(os.environ, TERNCHAIN_NET=net, TERNCHAIN_RELAY=relay)
env.pop("TERNCHAIN_DIR", None)
procs = []


def spawn(*a, out=None):
    p = subprocess.Popen([T, *a], env=env, cwd=work, stdout=out or open(os.path.join(work, a[0] + ".log"), "a"),
                         stderr=subprocess.STDOUT if out is None else open(os.path.join(work, "watch.err"), "a"), text=True)
    procs.append(p)
    return p


def run(*a, ok=True):
    r = subprocess.run([T, *a], env=env, cwd=work, capture_output=True, text=True)
    if ok and r.returncode != 0:
        sys.exit(f"{' '.join(a)}: {r.stderr or r.stdout}")
    return r


def cleanup():
    for p in procs:
        p.terminate()
    for p in procs:
        try:
            p.wait(5)
        except Exception:
            p.kill()
    if not args.keep:
        shutil.rmtree(work, ignore_errors=True)


try:
    spawn("devnet", "--net", net, "--listen", f"127.0.0.1:{port}", "--admin", f"127.0.0.1:{port + 1}")
    for _ in range(300):
        if os.path.exists(net) and os.path.getsize(net) > 0:
            break
        time.sleep(0.1)
    time.sleep(1)

    def owner(n):
        run("owner", "init", "--dir", f"id/{n}", "--name", n, "--guardian-dir", f"usb/{n}")

    owner("nodeco")
    spawn("relay", "serve", "--da", "--dir", "id/nodeco", "--listen", f"127.0.0.1:{port + 2}", "--store", "store")
    time.sleep(2)
    for n in ("alice", "bob", "carol"):
        owner(n)
    run("bank", "deposit", "--dir", "id/alice", "20")
    run("agent", "init", "--dir", "id/ada.alice", "--name", "ada", "--owner", "alice")
    run("owner", "grant", "--dir", "id/alice", "id/ada.alice/request.json", "--spend", "10", "--scopes", '{"free_pay":"0.01"}', "--gas", "1")
    run("agent", "join", "--dir", "id/ada.alice", "id/ada.alice/bundle.json")
    run("contact", "--dir", "id/bob", "alice")
    run("send", "--dir", "id/carol", "--to", "ada.alice", "hello from a stranger")
    for _ in range(30):  # ada reads it: tainted, so paying bob needs alice's approval
        r = run("inbox", "--dir", "id/ada.alice")
        if "carol" in r.stdout:
            break
        time.sleep(0.5)
    if args.channel:
        run("channel", "open", "--dir", "id/alice", "bob", "1", "--hours", "24")

    lines = queue.Queue()
    w = spawn("watch", "--dir", "id/bob", out=subprocess.PIPE)

    def reader():
        for line in w.stdout:
            lines.put((time.monotonic(), line))

    threading.Thread(target=reader, daemon=True).start()

    def wait_for(pred, t0, limit=60):
        while True:
            left = limit - (time.monotonic() - t0)
            if left <= 0:
                return None
            try:
                t, line = lines.get(timeout=left)
            except queue.Empty:
                return None
            try:
                e = json.loads(line)
            except ValueError:
                continue
            if pred(e):
                return t

    def drain():
        time.sleep(1.5 + random.random())  # a random phase against the 1 s blocks
        while not lines.empty():
            lines.get()

    # warm up: the watch's first round only learns the past
    time.sleep(3)
    run("send", "--dir", "id/alice", "--to", "bob", "warm-up")
    wait_for(lambda e: e.get("type") == "mail", time.monotonic(), 30)
    drain()

    res = {}

    def measure(name, act, pred):
        xs = []
        for i in range(args.runs):
            drain()
            t0 = time.monotonic()
            tag = f"{name}-{i}-{random.randint(0, 1 << 30)}"
            if not act(tag):
                print(f"{name} run {i}: the command failed", file=sys.stderr)
                continue
            t1 = time.monotonic() if pred is None else wait_for(lambda e: pred(e, tag), t0)
            if t1 is None:
                print(f"{name} run {i}: nothing seen in 60 s", file=sys.stderr)
                continue
            xs.append(t1 - t0)
        xs.sort()
        q = lambda p: xs[min(len(xs) - 1, int(round(p * (len(xs) - 1))))] if xs else None
        res[name] = {"n": len(xs), "p50": round(q(0.5), 3) if xs else None, "p95": round(q(0.95), 3) if xs else None,
                     "min": round(xs[0], 3) if xs else None, "max": round(xs[-1], 3) if xs else None}
        print(name, res[name], file=sys.stderr)

    measure("mail", lambda tag: run("send", "--dir", "id/alice", "--to", "bob", tag).returncode == 0,
            lambda e, tag: e.get("type") == "mail" and tag in e.get("text", ""))

    def approve(tag):
        t = time.monotonic()
        run("owner", "approve", "--dir", "id/alice", "ada.alice", "bob", "0.5")
        t1 = time.monotonic()
        r = run("bank", "pay", "--dir", "id/ada.alice", "bob", "0.5", tag, ok=False)
        if os.environ.get("PERF_DEBUG"):
            print(f"  approve {t1 - t:.3f}s, then pay {time.monotonic() - t1:.3f}s", file=sys.stderr)
        return r.returncode == 0

    measure("approve", approve, None)
    measure("pay", lambda tag: run("bank", "pay", "--dir", "id/alice", "bob", "0.001", tag).returncode == 0,
            lambda e, tag: e.get("type") == "paid" and "alice" in e.get("text", ""))
    if args.channel:
        measure("channel", lambda tag: run("channel", "pay", "--dir", "id/alice", "bob", "0.001").returncode == 0,
                lambda e, tag: e.get("type") == "channel-paid")
    print(json.dumps(res, indent=1))
finally:
    cleanup()
