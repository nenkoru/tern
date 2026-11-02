#!/usr/bin/env bash
# Red-team scenarios A-I against the integrated tern (in-process simulated chain,
# no network). The tests live in cmd/ternchain as redteam_*_test.go (package main
# needs them there) behind the build tag `redteam`, so `go test ./...` skips them.
# Each test passes when the code behaves as recorded in redteam/REPORT.md:
# a "FINDING" assertion flips (fails) once its fix lands.
#   redteam/run.sh            all scenarios
#   redteam/run.sh ab|c|d|e|fg|h|i   one scenario group
set -euo pipefail
cd "$(dirname "$0")/.."
tag=redteam
[ $# -gt 0 ] && tag="redteam_$1"
exec go test -tags "$tag" -run TestRedTeam -count=1 -v ./cmd/ternchain
