#!/usr/bin/env bash
# A cluster of friends, on a local devnet: three people's agents in a private
# group pool their machines to run one model split into 32 positions, each
# gets its own chat, a laptop closes and another machine picks up its part, a
# lazy node is caught by a check, and the group runs on credits instead of
# money. The compute is a stand-in engine (internal/stubengine); tern carries
# the membership, offers, presence, receipts, credits and checks (module coop,
# docs/PRIMITIVES.md "Pooling anything"). GPUs are one case: demo-quota.sh and
# demo-backup.sh use the same bricks for an API key and family backups.
source "$(dirname "$0")/coop-lib.sh"
coop_start 8745
go build -o "$B/stubengine" ./examples/cluster/stubengine
E="$B/stubengine"
coop_people box oscar nina ada
coop_group friends oscar box oscar nina ada

step "Each machine runs an engine holding some positions, offers them and announces itself (presence)"
pieces() { case $1 in oscar) echo 0-10 ;; nina) echo 11-21 ;; ada) echo 22-31 ;; esac; }
port() { case $1 in oscar) echo 9101 ;; nina) echo 9102 ;; ada) echo 9103 ;; esac; }
cap() { # NAME POSITIONS
	offer "$W/box.$1" gpu "stub-32 positions $2" '"whole":"stub-32","holds":"'"$2"'","of":32,"amount":{"value":8,"unit":"gb-vram"},"free_for":["friends"],"settle":["credits"],"protocol":"stubengine/1","region":"eu-central"'
}
for p in oscar nina ada; do
	lazy=0
	[ "$p" = nina ] && lazy=2 # nina's node skips the work on every second job (and claims it)
	"$E" serve --listen "127.0.0.1:$(port $p)" --pieces "$(pieces $p)" --of 32 --lazy $lazy 2>>"$W/engines.log" &
	cap "$p" "$(pieces $p)"
	"$T" presence announce --dir "$W/box.$p" friends --every 10 --ttl 30 --reach "http://127.0.0.1:$(port $p)" >/dev/null 2>>"$W/presence.log" &
	eval "LOOP_$p=$!"
done
sleep 2
"$T" presence list --dir "$W/box.oscar" friends | tee "$W/online.json" |
	j '"\n".join("  %-10s %-6s positions %s" % (m["name"], m["state"], m["offers"][0]["holds"]) for m in d["online"]) + "\n  missing: %r; held by one only: %r" % (d["coverage"]["stub-32"]["missing"], d["coverage"]["stub-32"]["thin"])'

chat() { # CONSUMER JOB [AVOID]: a chat through the holders online; receipts countersigned from the consumer's own count; credits
	"$T" presence list --dir "$W/box.$1" friends >"$W/online.json"
	"$E" run --presence "$W/online.json" --model stub-32 --job "$2" --prompt "hello from $1" --tokens 8 --avoid "${3:-}" >"$W/$2.json"
	j '"  %s: tokens %s via %s" % (d["job"], " ".join(d["tokens"]), ", ".join("%s[%d-%d]" % (s["name"], s["from"], s["to"]) for s in d["route"]))' <"$W/$2.json"
	python3 -c 'import json,sys; [print(s["name"], s["units"], s["from"], s["to"]) for s in json.load(open(sys.argv[1]))["route"]]' "$W/$2.json" |
		while read -r who units from to; do
			p="${who#box.}"
			[ "$p" = "$1" ] && continue # your own machine's work is not a credit
			"$T" receipt issue --dir "$W/box.$p" "box.$1" "$units" --unit layer-token --group friends --resource "stub-32 $from-$to" >"$W/r.json"
			"$T" receipt accept --dir "$W/box.$1" @"$W/r.json" --observed "$units" >"$W/rs.json"
			"$T" credits post --dir "$W/box.$p" friends @"$W/rs.json" >/dev/null
			echo "    $who served box.$1 $units layer-tokens: receipt countersigned, posted for credits"
		done
}

step "Everyone gets their own chat"
chat oscar chat-1
chat ada chat-2

step "Ada checks nina's part of her chat: two other machines load those positions and run the same pass (verify)"
read -r FROM TO IN OUT < <(python3 -c 'import json,sys; s=[x for x in json.load(open(sys.argv[1]))["route"] if x["name"]=="box.nina"][0]; print(s["from"], s["to"], s["passes"][0]["input"], s["passes"][0]["output"])' "$W/chat-2.json")
JOB="chat-2:$FROM-$TO:t0"
"$T" verify sample --dir "$W/box.ada" "$JOB" --seed "ada's secret" --rate 1 | j '"  sample: check=%s (seed committed as %s…)" % (d["check"], d["seed_commit"][:12])'
"$T" verify statement --dir "$W/box.nina" "$JOB" "$IN" "$OUT" --canon stub/sha256 >"$W/s-nina.json"
for p in oscar ada; do
	"$E" load --reach "http://127.0.0.1:$(port $p)" --pieces "$FROM-$TO" >/dev/null
	"$E" forward --reach "http://127.0.0.1:$(port $p)" --job "check-$JOB" --state "$IN" --from "$FROM" --to "$TO" >"$W/f.json"
	"$T" verify statement --dir "$W/box.$p" "$JOB" "$IN" "$(j 'd["output"]' <"$W/f.json")" --canon stub/sha256 >"$W/s-$p.json"
done
"$T" verify compare --dir "$W/box.ada" @"$W/s-nina.json" @"$W/s-oscar.json" @"$W/s-ada.json" | tee "$W/check.json" |
	j '"  verdict: %s; outvoted: %s" % (d["verdict"], ", ".join(d.get("outvoted") or []))'
"$T" verify report --dir "$W/box.ada" @"$W/check.json" --group friends >/dev/null
echo "  reported to the group (sealed). A provider with a bond would face a panel: verify bond / verify escalate"

step "Ada's laptop closes: her presence says 'leaving', nobody holds 22-31"
kill -TERM "$LOOP_ada"
wait "$LOOP_ada" 2>/dev/null || true
"$T" presence list --dir "$W/box.oscar" friends | j '"  online: %s; leaving: %s; missing: %r" % (", ".join(m["name"] for m in d["online"]), ", ".join(m["name"] for m in d["leaving"]), d["coverage"]["stub-32"]["missing"])'

step "Oscar's machine picks up the missing positions and says so"
"$E" load --reach "http://127.0.0.1:$(port oscar)" --pieces 22-31 >/dev/null
cap oscar 0-31
"$T" presence announce --dir "$W/box.oscar" friends --ttl 30 --reach "http://127.0.0.1:$(port oscar)" >/dev/null
"$T" presence list --dir "$W/box.nina" friends | j '"  missing now: %r" % d["coverage"]["stub-32"]["missing"]'

step "Nina chats; the group routes around the node that was outvoted"
chat nina chat-3 box.nina

step "No money: credits. Oscar (the group's admin) prices a layer-token at 0.1 credit; a floor of -5"
"$T" credits policy --dir "$W/oscar" friends '{"rates":{"layer-token":0.1},"floor":-5}' >/dev/null
sleep 3
"$T" pool order --dir "$W/box.nina" friends | j '"  rule %s:\n" % d["rule"] + "\n".join("  %d. %-6s %6.1f%s" % (q["rank"], q["name"], q["balance"], "  (waits: past the floor)" if q.get("waits") else "") for q in d["queue"])'
"$T" verify score --dir "$W/box.oscar" box.nina --group friends | j '"  box.nina'"'"'s checks: passed %d, failed %d" % (d["passed"], d["failed"])'

step "Done. Presence lived only in the data node's memory, sealed; receipts, credits and the check are sealed group posts"
echo "  (everything was in $W, removed now; the real-engine version: examples/cluster/README.md)"
