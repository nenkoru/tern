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
	"strings"
	"sync/atomic"
	"time"

	"github.com/nenkoru/tern/internal/scan"
)

// index.html: the whole page (styles and script inline, nothing from elsewhere).
//
//go:embed index.html
var indexHTML []byte

// pageOf: what the page fetches: the scan, plus what the text views compute.
// Not the ids and addrs tables (id or key => name): the page doesn't use
// them, and a served lookup from keys to names is more than it needs to show.
func pageOf(s *scan.State, every time.Duration, err error) []byte {
	b, _ := json.Marshal(s)
	var p map[string]json.RawMessage
	json.Unmarshal(b, &p)
	delete(p, "ids")
	delete(p, "addrs")
	var order []string
	for _, n := range sortedNames(s) {
		order = append(order, n.Name)
	}
	set := func(k string, v any) { b, _ := json.Marshal(v); p[k] = b }
	set("order", order)                               // names: owners in the order they came, each followed by its agents
	set("swap_rows", swapRows(s, newest(s.Swaps, 0))) // both sides and the price
	set("every_seconds", int64(every/time.Second))    // how often the server reads new blocks (0: never, --offline)
	if err != nil {
		set("error", scan.Redact(err).Error())
	}
	b, _ = json.Marshal(p)
	return b
}

// handler serves the page and its data. Everything shown comes from the
// chain, written by anyone: the page puts it in as text, never as markup,
// and the policy below lets nothing load from elsewhere.
//
// Only requests for a loopback name, or for the host --listen names, are
// answered: a web page elsewhere can't point a name of its own at 127.0.0.1
// (DNS rebinding) and read this one. When --listen is every interface
// (0.0.0.0, ::), an address typed as an IP is answered too.
func handler(data func() []byte, listen string) http.Handler {
	mux := http.NewServeMux()
	secure := func(w http.ResponseWriter) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
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
	return hostCheck(mux, listen)
}

// hostCheck: see handler.
func hostCheck(h http.Handler, listen string) http.Handler {
	lh, _, err := net.SplitHostPort(listen)
	if err != nil {
		lh = listen
	}
	lh = strings.ToLower(strings.Trim(lh, "[]"))
	all := lh == "" || lh == "0.0.0.0" || lh == "::"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
		ip := net.ParseIP(host)
		ok := host == "localhost" || strings.HasSuffix(host, ".localhost") || (ip != nil && ip.IsLoopback()) ||
			(host != "" && host == lh) || (all && ip != nil)
		if !ok {
			http.Error(w, "this scanner answers for 127.0.0.1 or its --listen host only", http.StatusMisdirectedRequest)
			return
		}
		h.ServeHTTP(w, r)
	})
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
	srv := &http.Server{Handler: handler(func() []byte { return *cur.Load() }, o.listen),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
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
