package scan

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A call that fails now and then is tried again; a refused range comes back
// at once (Update asks for a smaller one); one that keeps failing gives up.
func TestRetry(t *testing.T) {
	backoff = time.Millisecond
	defer func() { backoff = time.Second }()
	ctx := context.Background()

	n := 0
	v, err := retry(ctx, func() (int, error) {
		if n++; n < 3 {
			return 0, errors.New(`Post "https://rpc": net/http: TLS handshake timeout`)
		}
		return 7, nil
	})
	if v != 7 || err != nil || n != 3 {
		t.Fatalf("flaky: %d %v after %d calls", v, err, n)
	}

	n = 0
	_, err = retry(ctx, func() (int, error) { n++; return 0, errors.New("query exceeds max block range 10000") })
	if err == nil || n != 1 || !smaller(err) {
		t.Fatalf("too wide: %v after %d calls", err, n)
	}

	n = 0
	_, err = retry(ctx, func() (int, error) { n++; return 0, errors.New("502 bad gateway") })
	if err == nil || n != tries {
		t.Fatalf("down: %v after %d calls", err, n)
	}
}
