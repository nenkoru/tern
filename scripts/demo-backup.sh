#!/usr/bin/env bash
# Family backups: three households keep each other's encrypted backups. An
# archive is split into 8 chunks; each household's disk holds some; presence
# shows which chunks nobody holds and which only one does; a household that
# lost a chunk is caught by asking for it again and comparing hashes; disk
# space earns credits, and turns decide whose archive is stored next. The
# same bricks as the GPU cluster (demo-cluster.sh); no line of new code.
source "$(dirname "$0")/coop-lib.sh"
coop_start 8785
coop_people nas oscar nina ada
coop_group family oscar nas oscar nina ada
S="$W/disks"
mkdir -p "$S/oscar" "$S/nina" "$S/ada"

step "Oscar's photo archive, encrypted on his side (a stand-in: random bytes), in 8 chunks"
head -c 4000000 /dev/urandom >"$W/photos.enc"
split -b 500000 -d -a 1 "$W/photos.enc" "$W/chunk."
hash() { shasum -a 256 "$1" | awk '{print "0x"$1}'; }
for i in 0 1 2 3 4 5 6 7; do echo "$i $(hash "$W/chunk.$i")"; done >"$W/manifest"
echo "  8 chunks; oscar keeps the manifest of their hashes"

step "Each household offers disk (an offer of kind storage) and stores some chunks"
store() { # WHO CHUNKS…
	local who=$1
	shift
	for i in "$@"; do cp "$W/chunk.$i" "$S/$who/"; done
}
store nina 0 1 2 3 4
store ada 3 4 5 6
holds() { case $1 in nina) echo 0-4 ;; ada) echo 3-6 ;; esac; }
for p in nina ada; do
	offer "$W/nas.$p" storage "2 TB for the family's backups" '"whole":"backup:oscar-photos","holds":"'"$(holds $p)"'","of":8,"amount":{"value":2000,"unit":"gb"},"free_for":["family"],"settle":["credits"],"protocol":"sftp"'
	"$T" presence announce --dir "$W/nas.$p" family --ttl 120 >/dev/null
done
"$T" presence list --dir "$W/nas.oscar" family | j '"  missing: %r; held by one only: %r" % (d["coverage"]["backup:oscar-photos"]["missing"], d["coverage"]["backup:oscar-photos"]["thin"])'

step "Ada's disk picks up the missing chunk and copies of 0-2; nina's takes copies of 5-7"
store ada 0 1 2 7
store nina 5 6 7
offer "$W/nas.nina" storage "2 TB for the family's backups" '"whole":"backup:oscar-photos","holds":"0-7","of":8,"amount":{"value":2000,"unit":"gb"},"free_for":["family"],"settle":["credits"],"protocol":"sftp"'
"$T" presence announce --dir "$W/nas.nina" family --ttl 120 >/dev/null
offer "$W/nas.ada" storage "2 TB for the family's backups" '"whole":"backup:oscar-photos","holds":"0-7","of":8,"amount":{"value":2000,"unit":"gb"},"free_for":["family"],"settle":["credits"],"protocol":"sftp"'
"$T" presence announce --dir "$W/nas.ada" family --ttl 120 >/dev/null
"$T" presence list --dir "$W/nas.oscar" family | j '"  missing: %r; held by one only: %r" % (d["coverage"]["backup:oscar-photos"]["missing"], d["coverage"]["backup:oscar-photos"]["thin"])'

step "Months later nina's disk silently loses chunk 2; Oscar checks a chunk at random (verify)"
head -c 500000 /dev/zero >"$S/nina/chunk.2"
JOB="backup:oscar-photos:chunk-2"
IN=$(printf 'chunk 2 of backup:oscar-photos' | shasum -a 256 | awk '{print "0x"$1}')
"$T" verify sample --dir "$W/nas.oscar" "$JOB" --seed "oscar's secret 3" --rate 0.25 | j '"  sampled with a secret seed: check=%s" % d["check"]'
"$T" verify statement --dir "$W/nas.oscar" "$JOB" "$IN" "$(awk '$1==2{print $2}' "$W/manifest")" --canon sha256/bytes >"$W/s-oscar.json"
for p in nina ada; do
	"$T" verify statement --dir "$W/nas.$p" "$JOB" "$IN" "$(hash "$S/$p/chunk.2")" --canon sha256/bytes >"$W/s-$p.json"
done
"$T" verify compare --dir "$W/nas.oscar" @"$W/s-oscar.json" @"$W/s-nina.json" @"$W/s-ada.json" | tee "$W/check.json" |
	j '"  verdict: %s; outvoted: %s" % (d["verdict"], ", ".join(d.get("outvoted") or []))'
"$T" verify report --dir "$W/nas.oscar" @"$W/check.json" --group family >/dev/null
cp "$S/ada/chunk.2" "$S/nina/chunk.2"
echo "  reported to the family; nina re-fetches chunk 2 from ada"

step "Disk space earns credits: a receipt per household, in gb-months, countersigned by Oscar"
receipt "$W/nas.nina" nas.oscar "$W/nas.oscar" 4 gb-month family 4
receipt "$W/nas.ada" nas.oscar "$W/nas.oscar" 4 gb-month family 4
"$T" credits policy --dir "$W/oscar" family '{"rates":{"gb-month":10},"floor":-100}' >/dev/null
"$T" pool rule --dir "$W/oscar" family pool-round-robin@1 >/dev/null
sleep 3
"$T" credits show --dir "$W/nas.nina" family | j '"  credits: " + ", ".join("%s %+.0f" % kv for kv in sorted(d["balances"].items()))'
"$T" pool order --dir "$W/nas.nina" family nas.oscar nas.nina nas.ada | j '"  whose archive is stored next (rule %s): " % d["rule"] + ", ".join(q["name"] for q in d["queue"])'

step "Done: backups are one more thing a family pools; the bricks didn't change"
