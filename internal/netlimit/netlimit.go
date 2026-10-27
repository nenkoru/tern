// Who is asking, for the faucet's and the relay's per-client limits.
//
// A client is its address block, not its address: an IPv6 user gets a /64
// (often a /56 or a /48) from its provider and can pick any address in it, so
// limits count per /64, and its /56 and /48 have larger shares (Buckets); IPv4
// counts per address (/32). X-Forwarded-For and X-Real-IP are believed only
// from the configured reverse proxies (--trusted-proxy; default: none, since a
// local forwarder that adds no header, a Tor onion service or an ssh tunnel,
// would pass the client's own), read from the right: the first address that
// isn't one of them is the client, so a client can't forge its way in by
// sending the header itself.
package netlimit

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// DefaultTrustedProxies: none (audit 5 HM-10). A reverse proxy on this
// machine is LocalProxies: --trusted-proxy 127.0.0.1 (or the constant's value).
const DefaultTrustedProxies = "none"

// LocalProxies: this machine (a reverse proxy on it that sets X-Forwarded-For).
const LocalProxies = "127.0.0.0/8,::1/128"

var trustedProxies = mustCIDRs(DefaultTrustedProxies)

func mustCIDRs(s string) []*net.IPNet {
	n, err := parseCIDRs(s)
	if err != nil {
		panic(err)
	}
	return n
}

// parseCIDRs: "10.0.0.1,fd00::/8": addresses (a host each) or blocks; "none": nobody.
func parseCIDRs(s string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	if strings.TrimSpace(s) == "none" {
		return out, nil
	}
	for _, x := range strings.Split(s, ",") {
		x = strings.TrimSpace(x)
		if x == "" {
			continue
		}
		if !strings.Contains(x, "/") {
			ip := net.ParseIP(x)
			if ip == nil {
				return nil, fmt.Errorf("--trusted-proxy %q: an address or a block (CIDR)", x)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(x)
		if err != nil {
			return nil, fmt.Errorf("--trusted-proxy %q: %w", x, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// SetTrustedProxies: the reverse proxies whose X-Forwarded-For counts ("" keeps the default).
func SetTrustedProxies(s string) error {
	if s == "" {
		return nil
	}
	n, err := parseCIDRs(s)
	if err != nil {
		return err
	}
	trustedProxies = n
	return nil
}

func isTrustedProxy(ip net.IP) bool {
	for _, n := range trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP: the peer, or behind a trusted reverse proxy the client it names.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !isTrustedProxy(ip) {
		return host
	}
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		parts := strings.Split(x, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			p := net.ParseIP(strings.TrimSpace(parts[i]))
			if p == nil {
				return host // garbage from the client: count the proxy's peer, not a forged name
			}
			if !isTrustedProxy(p) || i == 0 {
				return p.String()
			}
		}
	}
	if x := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); x != nil {
		return x.String()
	}
	return host
}

// Buckets: every block a per-client limit counts against, with its share of
// the limit (× share): an IPv4 address (×1); an IPv6 /64 (×1), its /56 (×2) and
// its /48 (×4), so a /48's 65536 /64s get 4 shares, not 65536.
func Buckets(s string) map[string]int {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() != nil {
		return map[string]int{Bucket(s): 1}
	}
	return map[string]int{Bucket(s): 1, ip.Mask(net.CIDRMask(56, 128)).String() + "/56": 2, ip.Mask(net.CIDRMask(48, 128)).String() + "/48": 4}
}

// Bucket: what a per-client limit counts: an IPv4 address, or an IPv6 /64.
func Bucket(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return s
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String() + "/32"
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
