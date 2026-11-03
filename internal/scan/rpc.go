package scan

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// How an RPC says no, in the words providers use. The same patterns as the
// client's (cmd/ternchain/hostchain.go: rangeErrRE, rateErrRE, rangeLimitRE),
// copied because internal/ can't import a command; rateErrRE adds quota and
// credit wording. A rate limit is never a reason to ask for a smaller range:
// a narrower window would only mean more requests.
var (
	rangeErrRE   = regexp.MustCompile(`(?i)block range|range (?:is )?too (?:large|wide|big)|too many (?:blocks|results|logs)|more than \d+ (?:results|logs)|exceed|not supported|limit`)
	rateErrRE    = regexp.MustCompile(`(?i)429|too many requests|rate.?limit|capacity|throttl|compute units|quota|credits|request limit`)
	rangeLimitRE = regexp.MustCompile(`(?:limited to a|up to a|max(?:imum)? (?:block )?range(?: of)?|ranges over) (\d+)`)
	// revertRE: the call itself fails (a view that reverts, a token without
	// symbol()): asking again gives the same answer.
	revertRE = regexp.MustCompile(`(?i)execution reverted|invalid opcode`)
)

// rateLimited: the RPC asked us to slow down.
func rateLimited(err error) bool { return err != nil && rateErrRE.MatchString(err.Error()) }

// tooWide: the RPC refused the range or the number of results (not a rate limit).
func tooWide(err error) bool {
	if err == nil || rateLimited(err) {
		return false
	}
	return rangeErrRE.MatchString(err.Error()) || rangeLimitRE.MatchString(err.Error())
}

// reverted: a call that fails the same way every time.
func reverted(err error) bool { return err != nil && revertRE.MatchString(err.Error()) }

// Dial connects to an RPC whose HTTP requests are retried on 429 and 5xx
// answers, waiting as long as Retry-After says (up to 30 s), else 1, 2, 4…
// seconds. The client does the same (cmd/ternchain/hostchain.go retryRPC).
func Dial(ctx context.Context, u string) (*ethclient.Client, error) {
	c, err := rpc.DialOptions(ctx, u, rpc.WithHTTPClient(&http.Client{Transport: retryTransport{}, Timeout: 2 * time.Minute}))
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(c), nil
}

// retryTransport: an http.RoundTripper that retries what a busy RPC answers.
type retryTransport struct{}

const httpTries = 5

var httpBackoff = time.Second // the first wait without Retry-After; it doubles (tests shorten it)

func (retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for try := 0; ; try++ {
		if try > 0 && req.GetBody != nil {
			b, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = b
		}
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil || try >= httpTries-1 || req.GetBody == nil {
			return resp, err
		}
		switch resp.StatusCode {
		case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		default:
			return resp, nil
		}
		wait := httpBackoff << try
		if s, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && s > 0 && s <= 30 {
			wait = time.Duration(s) * time.Second
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
	}
}

// urlRE: a URL inside an error's text (Go's HTTP errors quote the whole URL).
var urlRE = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>]+`)

// Origin: a URL as scheme://host, without its path, query or user (where an
// RPC provider's key goes).
func Origin(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return "(an rpc)"
	}
	return p.Scheme + "://" + p.Host
}

// Redact: an error with every URL in it cut down to its Origin, so an RPC key
// never reaches a log, a page or a terminal.
func Redact(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	r := urlRE.ReplaceAllStringFunc(s, Origin)
	if r == s {
		return err
	}
	return redacted{r, err}
}

type redacted struct {
	msg string
	err error
}

func (r redacted) Error() string { return r.msg }

// Is keeps errors.Is working (context.Canceled, …) without Unwrap, which
// would hand the unredacted text back to whoever prints it.
func (r redacted) Is(target error) bool { return errors.Is(r.err, target) }
