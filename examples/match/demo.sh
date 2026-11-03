#!/usr/bin/env bash
# Match: dating through people's agents, on a fresh local devnet
# (examples/match/README.md): a community with a moderator, four people's
# agents, profiles posted local-only, scoring (a stub, or a local model's
# command in SCORER), a like the owner approves, a mutual like, a report and
# a ban, a profile matchtool refuses, a like over the daily cap, and the
# photos sent and received by the people themselves.
#
#   bash examples/match/demo.sh                               # stub scorer
#   SCORER='ollama run llama3.2' bash examples/match/demo.sh  # a local model scores
#
# Everything it makes (binaries, identities with their keys, the chain's
# files) goes to examples/match/work/ (git-ignored, safe to delete), or $W.
set -euo pipefail
R="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$R/../.." && pwd)"
W="${W:-$R/work}"
rm -rf "${W:?}"
mkdir -p "$W/bin" "$W/home"
(cd "$ROOT" && go build -o "$W/bin/ternchain" ./cmd/ternchain && go build -tags ops -o "$W/bin/ternchain-ops" ./cmd/ternchain &&
	go build -o "$W/bin/matchtool" ./examples/match/matchtool)
export PATH="$W/bin:$PATH" TERNCHAIN_BIN="$W/bin/ternchain" TERNCHAIN_NET="$W/net.json"
unset TERNCHAIN_RELAY TERNCHAIN_DIR TERNCHAIN_MODEL
PORT=${PORT:-8755}
SCORER=${SCORER:-}
cleanup() { kill $(jobs -p) 2>/dev/null || true; wait 2>/dev/null || true; }
trap cleanup EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
q() { ternchain "$@" >/dev/null; }
# expect PATTERN CMD…: CMD succeeds and prints PATTERN (fixed string); its output is kept in $OUT
expect() {
	local want=$1
	shift
	if ! OUT=$("$@" 2>&1); then
		echo "FAILED: $*"
		echo "$OUT"
		exit 1
	fi
	if ! grep -qF -- "$want" <<<"$OUT"; then
		echo "FAILED: $* did not print: $want"
		echo "$OUT"
		exit 1
	fi
}
# refused PATTERN CMD…: CMD fails, saying PATTERN
refused() {
	local want=$1
	shift
	if OUT=$("$@" 2>&1); then
		echo "FAILED: $* went through"
		echo "$OUT"
		exit 1
	fi
	grep -qF -- "$want" <<<"$OUT" || { echo "FAILED: $* failed without saying: $want"; echo "$OUT"; exit 1; }
}
say() { echo "  $*"; }
pick() { grep -o "\"$1\": [^,]*" <<<"$OUT" | head -${2:-1} | sed 's/^/  /'; }
# notproposed: the candidates' "not proposed" list of the last output, one line each
notproposed() { sed -n '/"not_proposed"/,/"skipped"/p' <<<"$OUT" | grep -oE '"(name|why)": "[^"]*"' | paste -d' ' - - | sed 's/^/  not proposed: /'; }

step "A fresh devnet"
"$W/bin/ternchain-ops" devnet --listen 127.0.0.1:$PORT --admin 127.0.0.1:$((PORT + 1)) >"$W/devnet.log" 2>&1 &
for _ in $(seq 150); do [ -s "$TERNCHAIN_NET" ] && break; sleep 0.2; done
[ -s "$TERNCHAIN_NET" ] || { cat "$W/devnet.log"; exit 1; }
sleep 1
cd "$W/home"
cp "$R"/try/* .
say "devnet 127.0.0.1:$PORT; the example's files in $W/home"

step "Five people, each with an agent ai.NAME (as AGENTS.md makes them)"
for p in pat nina oscar mallory lev; do
	q owner init --dir id/$p --name $p --guardian-dir guardian/$p
	q bank deposit --dir id/$p 5
	q agent init --dir agent/ai.$p --name ai --owner $p
	q owner grant --dir id/$p agent/ai.$p/request.json --spend 3 --scopes '{"free_pay":"0.3"}' --gas 0.5
	q agent join --dir agent/ai.$p agent/ai.$p/bundle.json
done
say "pat (the moderator), nina, oscar, mallory, lev"

step "The community mersin-dating: 18+ in its rules, joined by request"
q commons found --dir id/pat mersin-dating --rules "Dating in Mersin, 18+ only: no minors, ever. Profiles are tern.match/1 attachments posted local-only, with no names, contacts or photos. Photos and contacts go by sealed mail after a mutual like, to that one person only. Nobody asks anyone for money, gifts or crypto: report it."
expect mersin-dating ternchain find --dir agent/ai.nina dating
say "nina's agent found it; it read outside text, so joining needs nina's OK"
refused "approval" ternchain commons request --dir agent/ai.nina mersin-dating
q owner approve --dir id/nina ai.nina group:mersin-dating
for p in nina oscar mallory lev; do q commons request --dir agent/ai.$p mersin-dating; done
for p in nina oscar mallory lev; do q commons admit --dir id/pat mersin-dating ai.$p; done
for p in nina oscar lev; do q blocklist follow --dir agent/ai.$p mersin-dating --owners; done
say "pat admitted four agents; three follow the community's bans"

step "Profiles: checked, posted as local-only attachments"
q model set local --dir agent/ai.nina --owner-dir id/nina
q model set local --dir agent/ai.lev --owner-dir id/lev
expect '"valid": true' matchtool check nina.profile.json nina.prefs.json oscar.profile.json oscar.prefs.json
cat >lev.draft.json <<'EOF'
{"schema":"tern.match/1","adult":true,"age":36,"region":"TR-33","area":"Mersin","gender":"man","interests":["sailing","smoking","chess"],
 "seeking":{"genders":["woman"],"age_min":28,"age_max":40,"intent":"long-term"},"about":"Sailor, fond of chess. Call me: +90 532 000 00 00"}
EOF
refused "no contacts" matchtool post --dir agent/ai.lev mersin-dating lev.draft.json
say "lev's draft refused: $(grep -o 'no contacts in a profile ([^)]*)' <<<"$OUT" | head -1)"
sed 's/ Call me: +90 532 000 00 00//' lev.draft.json >lev.profile.json
expect '"posted"' matchtool post --dir agent/ai.lev mersin-dating lev.profile.json
expect '"posted"' matchtool post --dir agent/ai.nina mersin-dating nina.profile.json
q group post --dir agent/ai.mallory mersin-dating "profile (tern.match/1)" --attach mallory.profile.json --local-only
say "nina, lev (fixed) and mallory (plain ternchain, unchecked) posted"

step "Oscar's agent: a cloud model by default, so the profiles are withheld; then Oscar declares a local one"
expect "withheld: local-model only" ternchain group read --dir agent/ai.oscar mersin-dating
refused "withheld" matchtool candidates --dir agent/ai.oscar mersin-dating --me oscar.profile.json --prefs oscar.prefs.json
q model set local --dir agent/ai.oscar --owner-dir id/oscar
expect '"posted"' matchtool post --dir agent/ai.oscar mersin-dating oscar.profile.json
expect '"name": "ai.nina"' matchtool candidates --dir agent/ai.oscar mersin-dating --me oscar.profile.json --prefs oscar.prefs.json ${SCORER:+--scorer "$SCORER"}
say "proposed to oscar:"; pick name; pick score
notproposed

step "Oscar likes Nina: refused until Oscar approves that one name"
refused "your person approves this like first" matchtool like --dir agent/ai.oscar --community mersin-dating --prefs oscar.prefs.json ai.nina "We both hike and play chess, and we both want something long-term."
say "$(grep -o 'they run [^"]*' <<<"$OUT")"
q owner approve --dir id/oscar ai.oscar ai.nina
expect '"liked": "ai.nina"' matchtool like --dir agent/ai.oscar --community mersin-dating --prefs oscar.prefs.json ai.nina "We both hike and play chess, and we both want something long-term."
pick likes_today; pick cap

step "Nina's agent: the like, her candidates (with a scam warning), her like back: mutual"
expect '"name": "ai.oscar"' matchtool matches --dir agent/ai.nina
say "liked her: ai.oscar ($(grep -o '"reason": "[^"]*"' <<<"$OUT" | head -1))"
expect "romance scam" matchtool candidates --dir agent/ai.nina mersin-dating --me nina.profile.json --prefs nina.prefs.json ${SCORER:+--scorer "$SCORER"}
say "proposed to nina:"; pick name 1; pick score 1
notproposed
q owner approve --dir id/nina ai.nina ai.oscar
expect '"mutual": true' matchtool like --dir agent/ai.nina --community mersin-dating --prefs nina.prefs.json ai.oscar "Hikes, chess and old doors: yes, gladly."
say "mutual. next: $(grep -o '"next": "[^"]*' <<<"$OUT" | cut -c10-140)…"

step "The daily cap: with daily_likes 1, Nina's second like today never goes out"
sed 's/"daily_likes": 3/"daily_likes": 1/' nina.prefs.json >nina-strict.prefs.json
refused "daily cap" matchtool like --dir agent/ai.nina --community mersin-dating --prefs nina-strict.prefs.json ai.lev "Chess sometime?"
say "$(grep -o 'the daily cap[^(]*' <<<"$OUT")"

step "A report and a ban: Mallory's likes no longer reach the members who follow the bans"
expect '"reported": "ai.mallory"' matchtool report --dir agent/ai.nina mersin-dating ai.mallory "pitches crypto trading in his profile: a romance-scam pattern"
expect '"banned": true' ternchain commons ban --dir id/pat mersin-dating ai.mallory "romance scam: crypto trading pitch"
expect '"liked": "ai.nina"' matchtool like --dir agent/ai.mallory --community mersin-dating ai.nina "You seem special, I would love to know you better."
expect '"me": "ai.nina"' ternchain inbox --dir agent/ai.nina
if grep -q ai.mallory <<<"$OUT"; then echo "FAILED: a banned name's like reached nina's agent"; exit 1; fi
expect '"mutual"' matchtool matches --dir agent/ai.nina
if grep -q ai.mallory <<<"$OUT"; then echo "FAILED: a banned name's like counted"; exit 1; fi
say "mallory banned; his like was sealed and sent, and dropped by nina's client"

step "The photos: each person shrinks theirs (metadata dropped) and sends it with their own key, to the other only"
expect "EXIF" matchtool photo nina-original.jpg nina.jpg
say "nina.jpg: $(grep -o '"bytes": [0-9]*' <<<"$OUT") (from $(wc -c <nina-original.jpg | tr -d ' ') bytes)"
q send --dir id/nina --to oscar --local-only --attach nina.jpg "It's mutual! This photo is for you only. If you'd like to talk: Signal, nina.31"
expect "photos/nina-" matchtool save --dir id/oscar --from nina photos
q contact --dir id/oscar nina
expect "EXIF" matchtool photo oscar-original.jpg oscar.jpg
q send --dir id/oscar --to nina --local-only --attach oscar.jpg "Lovely to meet you. Mine, for you only. Signal works for me too."
expect "photos/oscar-" matchtool save --dir id/nina --from oscar photos
for p in nina oscar; do
	cmp -s "$p.jpg" photos/$p-*.jpg || { echo "FAILED: $p's photo changed on the way"; exit 1; }
	if grep -q Exif photos/$p-*.jpg; then echo "FAILED: $p's photo kept its EXIF"; exit 1; fi
done
say "both photos arrived whole, without metadata: $(ls photos | tr '\n' ' ')"

printf '\n\033[1m== demo finished: every step ran\033[0m\n'
