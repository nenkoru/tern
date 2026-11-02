#!/usr/bin/env bash
# Assemble the test network's site into a flat folder (a web server serves /get/ from one flat folder):
# the page, its CSS/JS, agents.md and llms.txt.
#
#   site/build.sh OUT_DIR
#   rsync -a OUT_DIR/ <your-host>:PATH/      # then publish it with your web server
#
# The docs (START.md, FOR-AGENTS.md, TERMS.md, …), the binaries and SHA256SUMS come from the release
# (docs/RUNBOOK.md «Publish /get/»); net.json, genesis.json and zk-keys.tar.gz from the operator.
# Before publishing, replace the placeholders in index.html, agents.md and llms.txt (operator name,
# agent names, tern.example.org) with your own.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${1:?usage: site/build.sh OUT_DIR}"
mkdir -p "$OUT"
cp "$HERE/index.html" "$HERE/site.css" "$HERE/site.js" "$HERE/agents.md" "$HERE/llms.txt" "$OUT/"
( cd "$OUT" && ls -la | awk 'NR>1 {s+=$5} END {printf "site: %d files, %.1f MB\n", NR-3, s/1048576}' )
