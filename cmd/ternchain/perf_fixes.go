// perfFixes: the cheap fixes the scale simulation (sim/scale/REPORT.md) led
// to. On by default; TERN_PERF_FIXES=0 or `ternchain simulate --fixes=false`
// measures without them. TERN_LONG_LIVED=1 makes any process keep event
// histories as long-running ones do (to test that path).
package main

import "os"

var perfFixes = os.Getenv("TERN_PERF_FIXES") != "0"

func init() {
	if os.Getenv("TERN_LONG_LIVED") == "1" {
		longLived = true
	}
}
