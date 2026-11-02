# Shared setup for the co-op demos (demo-cluster.sh, demo-quota.sh,
# demo-backup.sh): a devnet, a data node, people with an agent each, a
# private group. Sourced, not run. Needs go and python3.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
W="$(mktemp -d)"
B="$W/bin"
step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
j() { python3 -c 'import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))' "$1"; }
cleanup() {
	kill $(jobs -p) 2>/dev/null || true
	wait 2>/dev/null || true
	rm -rf "${W:?}"
}
trap cleanup EXIT

# coop_start PORT-BASE: build, a devnet on PORT-BASE (+1 admin), a data node on PORT-BASE+5.
coop_start() {
	step "Build ternchain and ternchain-ops; start a devnet and a data node (presence lives in its memory, never on chain)"
	go build -o "$B/ternchain" ./cmd/ternchain
	go build -tags ops -o "$B/ternchain-ops" ./cmd/ternchain
	T="$B/ternchain"
	export TERNCHAIN_NET="$W/net.json"
	local node="127.0.0.1:$(($1 + 5))"
	"$B/ternchain-ops" devnet --listen "127.0.0.1:$1" --admin "127.0.0.1:$(($1 + 1))" --relay-url "http://$node" 2>"$W/devnet.log" &
	for _ in $(seq 100); do [ -s "$TERNCHAIN_NET" ] && break; sleep 0.2; done
	"$B/ternchain-ops" relay serve --listen "$node" --da 2>"$W/node.log" &
	for _ in $(seq 100); do curl -sf "http://$node/info" >/dev/null && break; sleep 0.2; done
}

# coop_people AGENT NAME…: each person registers a name and grants an agent AGENT.NAME on their own machine.
coop_people() {
	local agent=$1
	shift
	step "People with an agent each: $*"
	for p in "$@"; do
		"$T" owner init --dir "$W/$p" --name "$p" --guardian-dir "$W/usb/$p" >/dev/null
		"$T" agent init --dir "$W/$agent.$p" --name "$agent" --owner "$p" >/dev/null
		"$T" owner grant --dir "$W/$p" "$W/$agent.$p/request.json" --spend 1 >/dev/null
		"$T" agent join --dir "$W/$agent.$p" "$W/$agent.$p/bundle.json" --no-announce >/dev/null
		"$T" modules enable coop --dir "$W/$agent.$p" >/dev/null
		"$T" modules enable coop --dir "$W/$p" >/dev/null
		echo "  $agent.$p"
	done
}

# coop_group GROUP ADMIN AGENT NAME…: ADMIN makes a private group; the agents join; ADMIN admits them (keys go sealed).
coop_group() {
	local group=$1 admin=$2 agent=$3
	shift 3
	step "$admin makes a private group '$group'; the agents ask to join; $admin admits them"
	"$T" group create --dir "$W/$admin" "$group" --private >/dev/null
	for p in "$@"; do "$T" group join --dir "$W/$agent.$p" "$group" >/dev/null; done
	local names=()
	for p in "$@"; do names+=("$agent.$p"); done
	"$T" group admit --dir "$W/$admin" "$group" "${names[@]}" >/dev/null
	for p in "$@"; do "$T" inbox --dir "$W/$agent.$p" >/dev/null; done
	echo "  admitted: $(printf '%s ' "$@")"
}

# offer DIR KIND TITLE [EXTRA-JSON-FIELDS]: keep an offer (tern.offer/1).
offer() {
	"$T" offer set --dir "$1" '{"schema":"tern.offer/1","kind":"'"$2"'","title":"'"$3"'"'"${4:+,$4}"'}' >/dev/null
}

# receipt PROVIDER-DIR CONSUMER CONSUMER-DIR COUNT UNIT GROUP OBSERVED [MORE-FLAGS]: issue, countersign
# from the consumer's own count, post for credits.
receipt() {
	"$T" receipt issue --dir "$1" "$2" "$4" --unit "$5" --group "$6" ${8:-} >"$W/r.json"
	"$T" receipt accept --dir "$3" @"$W/r.json" --observed "$7" >"$W/rs.json"
	"$T" credits post --dir "$1" "$6" @"$W/rs.json" >/dev/null
}
