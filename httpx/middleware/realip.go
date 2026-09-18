// Package middleware holds the HTTP middleware httpx assembles into a router,
// each usable on its own.
package middleware

import (
	"net"
	"net/http"
	"strings"
)

// RealIPOptions configures RealIP.
//
// The zero value ignores forwarding headers entirely, which is the only safe
// default: X-Forwarded-For is a request header like any other, and a service
// reachable directly will happily believe whatever a client writes in it.
type RealIPOptions struct {
	// TrustedProxies are the addresses and CIDR blocks of proxies in front of
	// this service. An entry in the header that matches one of these is a hop,
	// not a client.
	TrustedProxies []string

	// TrustAnyPeer honours the header whatever address the connection came
	// from. Set it when the platform guarantees nothing can reach the process
	// except its own front end, and the front end's address range is not
	// something you can enumerate — a managed container runtime, typically.
	//
	// It is a real loosening: if anything else can reach the port, that thing
	// can claim any client IP it likes.
	TrustAnyPeer bool

	// Header defaults to X-Forwarded-For. X-Real-IP is the other common
	// spelling; it carries a single address rather than a list, which this
	// handles.
	Header string
}

// RealIP rewrites r.RemoteAddr to the client's address, so that everything
// downstream — rate limiting, logging, abuse rules — sees the client rather
// than the proxy.
//
// The address is the rightmost entry in the forwarding header that is not a
// trusted proxy. Rightmost, not leftmost, and this is the whole point of the
// middleware.
//
// A forwarding header is a list that each hop appends to, so the leftmost entry
// is whatever the original client sent — including a client that sent a list of
// its own invention before any proxy ever saw the request. Trusting the
// leftmost entry (which is what the obvious implementation does, and what
// several widely used ones do) hands every client a per-request way to pick its
// own identity, and therefore its own rate-limit bucket. Walking from the right
// and stopping at the first address no trusted proxy could have written is the
// only reading that cannot be forged.
//
// When the header is not trusted — no trusted proxies configured, or a
// connection from an address that is not one of them — RemoteAddr is left
// exactly as it was.
func RealIP(opts RealIPOptions) func(http.Handler) http.Handler {
	header := opts.Header
	if header == "" {
		header = "X-Forwarded-For"
	}
	trusted := parseCIDRs(opts.TrustedProxies)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if opts.TrustAnyPeer || peerIsTrusted(r.RemoteAddr, trusted) {
				if ip := clientIP(r.Header.Values(header), trusted); ip != "" {
					r.RemoteAddr = net.JoinHostPort(ip, "0")
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// clientIP walks the forwarding entries from the right and returns the first
// address that is not a trusted proxy.
func clientIP(headers []string, trusted []*net.IPNet) string {
	var entries []string
	for _, h := range headers {
		for _, part := range strings.Split(h, ",") {
			if p := strings.TrimSpace(part); p != "" {
				entries = append(entries, p)
			}
		}
	}

	for i := len(entries) - 1; i >= 0; i-- {
		ip := parseIP(entries[i])
		if ip == nil {
			// An unparseable entry is not evidence of anything. Skipping it
			// rather than stopping means a proxy that writes a hostname, or a
			// client that writes rubbish, does not hide the entries behind it.
			continue
		}
		if inAny(ip, trusted) {
			continue
		}
		return ip.String()
	}
	return ""
}

func peerIsTrusted(remoteAddr string, trusted []*net.IPNet) bool {
	if len(trusted) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := parseIP(host)
	return ip != nil && inAny(ip, trusted)
}

// parseIP accepts a bare address or one with a port, which is what a proxy
// sometimes writes.
func parseIP(s string) net.IP {
	if ip := net.ParseIP(s); ip != nil {
		return ip
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return net.ParseIP(host)
	}
	return nil
}

// parseCIDRs accepts CIDR blocks and single addresses. An entry that is neither
// is dropped: a typo in a proxy list must not silently widen what is trusted,
// and dropping it narrows instead.
func parseCIDRs(entries []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, e := range entries {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if _, block, err := net.ParseCIDR(e); err == nil {
			nets = append(nets, block)
			continue
		}
		if ip := net.ParseIP(e); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return nets
}

func inAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
