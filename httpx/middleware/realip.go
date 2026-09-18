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
// The zero value ignores forwarding headers. X-Forwarded-For is a request
// header like any other, so a service reachable directly would otherwise
// believe whatever a client wrote in it.
type RealIPOptions struct {
	// TrustedProxies are the addresses and CIDR blocks of proxies in front of
	// this service. An entry in the header that matches one of these is a hop,
	// not a client.
	TrustedProxies []string

	// TrustAnyPeer honours the header whatever address the connection came
	// from. Set it on a managed container runtime, where nothing but the
	// platform's front end can reach the process and that front end's address
	// range is not enumerable.
	//
	// It is a genuine loosening: anything else that can reach the port can
	// claim any client address.
	TrustAnyPeer bool

	// Header defaults to X-Forwarded-For. X-Real-IP carries a single address
	// rather than a list, which this also handles.
	Header string
}

// RealIP rewrites r.RemoteAddr to the client's address, so that rate limiting,
// logging and abuse rules see the client rather than the proxy.
//
// The address is the rightmost entry in the forwarding header that is not a
// trusted proxy. Rightmost, not leftmost: each hop appends to the header, so
// the leftmost entry is whatever the original client sent, including a list it
// invented before any proxy saw the request. Reading from the left — which is
// what several widely used implementations do — lets a client choose its own
// rate-limit bucket on every request.
//
// When the header is not trusted, because no trusted proxies are configured or
// the connection came from an address that is not one of them, RemoteAddr is
// left unchanged.
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
			// Skip rather than stop, so a proxy that writes a hostname does not
			// hide the entries behind it.
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
// is dropped, so a typo in the proxy list narrows what is trusted rather than
// widening it.
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
