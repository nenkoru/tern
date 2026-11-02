package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/nenkoru/tern/internal/scan"
)

// index.html: the whole page (styles and script inline, nothing from elsewhere).
//
//go:embed index.html
var indexHTML []byte

// page is what the page fetches: the scan, plus what the text views compute.
type page struct {
	*scan.State
	Order   []string  `json:"order"` // names: owners in the order they came, each followed by its agents
	SwapRow []swapRow `json:"swap_rows"`
	Every   int64     `json:"every_seconds"` // how often the server reads new blocks (0: never, --offline)
	Error   string    `json:"error,omitempty"`
}

func pageOf(s *scan.State, every time.Duration, err error) []byte {
	p := page{State: s, SwapRow: swapRows(s, newest(s.Swaps, 0)), Every: int64(every / time.Second)}
	for _, n := range sortedNames(s) {
		p.Order = append(p.Order, n.Name)
	}
	if err != nil {
		p.Error = err.Error()
	}
	b, _ := json.Marshal(p)
	return b
}

// handler serves the page and its data. Everything shown comes from the
// chain, written by anyone: the page puts it in as text, never as markup,
// and the policy below lets nothing load from elsewhere.
func handler(data func() []byte) http.Handler {
	mux := http.NewServeMux()
	secure := func(w http.ResponseWriter) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /state.json", func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data())
	})
	return mux
}

// serve: the page, with the scan read again every o.every (unless --offline).
func serve(ctx context.Context, s *scan.State, n scan.Net, cache string, o opts, stale error) error {
	every := o.every
	if o.offline {
		every = 0
	} else if every < 5*time.Second {
		every = 5 * time.Second
	}
	var cur atomic.Pointer[[]byte]
	set := func(err error) { b := pageOf(s, every, err); cur.Store(&b) }
	set(stale)
	l, err := net.Listen("tcp", o.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: handler(func() []byte { return *cur.Load() }), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stderr, "serving %s at http://%s/ (Ctrl-C to stop)\n", s.Network, l.Addr())
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	if every > 0 {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				// The scan is changed only here; the page reads the bytes made after each update.
				err := update(ctx, s, n, cache, o.confirmations)
				if err != nil && ctx.Err() == nil {
					fmt.Fprintln(os.Stderr, "scanner: update:", err)
				}
				set(err)
			}
		}()
	}
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
