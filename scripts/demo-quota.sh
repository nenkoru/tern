#!/usr/bin/env bash
# A shared API key, split fairly: flatmates pool one paid key. It stays on
# the holder's machine behind a proxy (examples/quota/proxy.py, a stand-in);
# each call's tokens become a receipt the caller countersigns from its own
# count; the group runs on credits, and its queue rule is a daily quota. The
# same bricks as the GPU cluster (demo-cluster.sh); no line of new code.
source "$(dirname "$0")/coop-lib.sh"
coop_start 8765
coop_people key oscar nina ada
coop_group flat oscar key oscar nina ada
PORT=9201

step "Oscar offers his key's quota to the flat (an offer of kind api-quota) and runs the proxy"
python3 examples/quota/proxy.py $PORT &
offer "$W/key.oscar" api-quota "an LLM API key: 1M tokens a month, at cost" '"amount":{"value":1000000,"unit":"token/month"},"price":{"coins":"0.002","per":1000,"unit":"token"},"free_for":["flat"],"settle":["credits","channel"],"protocol":"openai-compatible/1"'
"$T" presence announce --dir "$W/key.oscar" flat --ttl 120 --reach "http://127.0.0.1:$PORT" >/dev/null
"$T" presence list --dir "$W/key.nina" flat | j '"\n".join("  %s offers %s (%s), at %s" % (m["name"], m["offers"][0]["title"], m["offers"][0]["kind"], m["reach"]) for m in d["online"])'

step "The flat's rules (Oscar is the admin): 1 credit per 100 tokens; queue by a daily quota of 5 credits each"
"$T" credits policy --dir "$W/oscar" flat '{"rates":{"token":0.01},"floor":-1000}' >/dev/null
"$T" pool rule --dir "$W/oscar" flat pool-quota@1 --quota-day 5 >/dev/null

call() { # WHO PROMPT: a call through the proxy; prints the tokens the caller's own side counted
	curl -sf "http://127.0.0.1:$PORT/v1/chat/completions" -H "X-Member: key.$1" -H 'Content-Type: application/json' \
		-d '{"model":"any","messages":[{"role":"user","content":"'"$2"'"}]}' | j 'd["usage"]["total_tokens"]'
}
step "Nina and Ada use it; each counts the tokens it was given"
NINA=0
for q in "summarise this contract" "translate the lease into Turkish, every clause" "draft a reply to the landlord" "and a shorter one"; do
	NINA=$((NINA + $(call nina "$q")))
done
ADA=$(call ada "what is a fair way to split the electricity bill")
echo "  nina: $NINA tokens, ada: $ADA tokens (their own counts)"
echo "  the proxy's counts: $(curl -sf http://127.0.0.1:$PORT/usage)"

step "Oscar's agent writes a receipt per member from the proxy's log; each countersigns up to its own count"
for p in nina ada; do
	n=$(curl -sf http://127.0.0.1:$PORT/usage | j 'd["key.'$p'"]')
	own=$([ $p = nina ] && echo $NINA || echo $ADA)
	receipt "$W/key.oscar" "key.$p" "$W/key.$p" "$n" token flat "$own" "--unit-price 0.002/1000 --resource api-key"
	echo "  key.$p: $n tokens, countersigned, posted for credits"
done
echo "  (a receipt above the member's own count is refused:)"
"$T" receipt issue --dir "$W/key.oscar" key.ada 99999 --unit token --group flat >"$W/r.json"
"$T" receipt accept --dir "$W/key.ada" @"$W/r.json" --observed "$ADA" 2>&1 | sed 's/^/    /' || true

sleep 3
step "Who goes next when the key's rate limit is busy: least used today first; past the quota, wait"
"$T" pool order --dir "$W/key.ada" flat key.nina key.ada | j '"  rule %s:\n" % d["rule"] + "\n".join("  %d. %s%s" % (q["rank"], q["name"], "  (waits: past today\x27s quota)" if q.get("waits") else "") for q in d["queue"])'
"$T" credits show --dir "$W/key.ada" flat | j '"  credits: " + ", ".join("%s %+.1f" % kv for kv in sorted(d["balances"].items()))'
echo "  (at cost instead of credits: 'receipt settle key.oscar' pays the receipts' price by channel voucher)"

step "Done: an API key is one more thing a group pools; the bricks didn't change"
