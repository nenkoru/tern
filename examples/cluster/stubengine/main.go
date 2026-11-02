// stubengine: the command of internal/stubengine, a stand-in for a real
// split-model engine in the "cluster of friends" demo (scripts/demo-cluster.sh).
//
//	stubengine serve --listen 127.0.0.1:9101 --pieces 0-10 --of 32 [--lazy N]   a node (--lazy N: every Nth job, no work done)
//	stubengine run --presence FILE --model M --job J --prompt TEXT [--tokens 8] [--avoid NAME,…]
//	                     a chat routed through the holders online (FILE: ternchain presence list's output)
//	stubengine forward --reach URL --job J --state 0x… --from A --to B         one segment again, for a spot check
//	stubengine load --reach URL --pieces 11-31                                 hold more pieces (a laptop closed)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/nenkoru/tern/internal/stubengine"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: stubengine serve|run|forward|load …")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	listen := fs.String("listen", "127.0.0.1:9101", "")
	pieces := fs.String("pieces", "", "")
	of := fs.Int("of", 32, "")
	lazy := fs.Int("lazy", 0, "")
	presence := fs.String("presence", "", "")
	model := fs.String("model", "", "")
	job := fs.String("job", "", "")
	prompt := fs.String("prompt", "", "")
	tokens := fs.Int("tokens", 8, "")
	avoid := fs.String("avoid", "", "")
	reach := fs.String("reach", "", "")
	stateS := fs.String("state", "", "")
	from := fs.Int("from", 0, "")
	to := fs.Int("to", 0, "")
	fs.Parse(os.Args[2:])
	enc := json.NewEncoder(os.Stdout)
	var err error
	switch os.Args[1] {
	case "serve":
		var n *stubengine.Node
		if n, err = stubengine.NewNode(*pieces, *of, *lazy); err == nil {
			log.Printf("stubengine: pieces %s of %d on %s", *pieces, *of, *listen)
			err = http.ListenAndServe(*listen, n.Handler())
		}
	case "run":
		var b []byte
		if b, err = os.ReadFile(*presence); err != nil {
			break
		}
		var p struct {
			Online []stubengine.Member `json:"online"`
		}
		if err = json.Unmarshal(b, &p); err != nil {
			err = fmt.Errorf("%s: not presence list output: %v", *presence, err)
			break
		}
		av := map[string]bool{}
		for _, a := range strings.Split(*avoid, ",") {
			av[strings.TrimSpace(a)] = true
		}
		var ch stubengine.Chat
		if ch, err = stubengine.Run(p.Online, *model, *job, *prompt, *tokens, av); err == nil {
			err = enc.Encode(ch)
		}
	case "forward":
		var st [32]byte
		if st, err = stubengine.ParseState(*stateS); err == nil {
			var p stubengine.Pass
			if p, err = stubengine.Forward(*reach, *job, st, *from, *to); err == nil {
				err = enc.Encode(p)
			}
		}
	case "load":
		if err = stubengine.LoadAt(*reach, *pieces); err == nil {
			err = enc.Encode(map[string]string{"loaded": *pieces, "at": *reach})
		}
	default:
		err = fmt.Errorf("unknown command %s", os.Args[1])
	}
	if err != nil {
		log.Fatal(err)
	}
}
