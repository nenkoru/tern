// matchtool: the dating example's helper (examples/match/README.md): checks
// profiles, reads and scores a community's profiles through ternchain, sends
// likes within a daily cap, finds mutual likes, shrinks photos. Build it from
// the repository: go build -o ~/tern/matchtool ./examples/match/matchtool
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/nenkoru/tern/examples/match"
)

func main() {
	if err := match.Main(os.Args[1:], match.ExecRunner(), os.Stdout); err != nil {
		b, _ := json.MarshalIndent(map[string]string{"error": err.Error()}, "", "  ")
		fmt.Println(string(b))
		os.Exit(1)
	}
}
