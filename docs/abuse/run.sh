#!/bin/sh
# The anti-abuse simulation (THREATS.md A–E) on a fresh local devnet
# (127.0.0.1:18545, admin :18546) and data node (:18650). Its state: docs/abuse/run/.
#
#   docs/abuse/run.sh scripted     fresh world: 20 people + helpers, 3 communities (one citizens only), a shop,
#                                 then the scripted farms and stand-ins for every scenario → run/results.{json,md}
#   docs/abuse/run.sh fixed        the same with the cheap fixes on (ABUSE_FIXES=1) → run/results-fixed.{json,md}
#   docs/abuse/run.sh serve        reopen the world for adaptive attackers (LLM agents with the CLI, net file
#                                 run/public/net.json, identities run/ids/NAME); people post, read and report,
#                                 helpers act naively on their mail; stop with: touch docs/abuse/run/stop
#   docs/abuse/run.sh measure      what the adaptive attackers got through → run/adaptive-results.json
set -e
cd "$(dirname "$0")/../.."
mkdir -p docs/abuse/run
phase=${1:-scripted}
fixes=0
[ "$phase" = fixed ] && { phase=scripted; fixes=1; }
ABUSE_PHASE=$phase ABUSE_FIXES=$fixes go test -tags abusesim -run TestAbuseSim -count=1 -timeout 4h -v ./cmd/ternchain 2>&1 | tee docs/abuse/run/$phase.log
