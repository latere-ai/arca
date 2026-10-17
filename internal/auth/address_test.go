// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package auth

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

// TestTheClientAddressIsForwardedOnlyByATrustedProxy: behind a trusted
// proxy the client is the last forwarded address outside the trusted
// ranges; from anywhere else, and with no range trusted, the header is
// ignored, so a caller cannot pick the address it is limited under.
func TestTheClientAddressIsForwardedOnlyByATrustedProxy(t *testing.T) {
	pods := []netip.Prefix{netip.MustParsePrefix("10.150.0.0/16")}
	for _, c := range []struct {
		name      string
		peer      string
		forwarded []string
		trusted   []netip.Prefix
		want      string
	}{
		{"no proxy", "203.0.113.7:4321", nil, pods, "203.0.113.7"},
		{"a trusted proxy names the client", "10.150.0.79:55000", []string{"198.51.100.4"}, pods, "198.51.100.4"},
		{"a client's own entry before the proxy's is passed over", "10.150.0.79:55000", []string{"192.0.2.1, 198.51.100.4"}, pods, "198.51.100.4"},
		{"a second trusted hop is walked past", "10.150.0.79:55000", []string{"198.51.100.4, 10.150.3.2"}, pods, "198.51.100.4"},
		{"entries across two headers", "10.150.0.79:55000", []string{"192.0.2.1", "198.51.100.4"}, pods, "198.51.100.4"},
		{"an untrusted peer's header is ignored", "203.0.113.7:4321", []string{"198.51.100.4"}, pods, "203.0.113.7"},
		{"no range trusts any header", "10.150.0.79:55000", []string{"198.51.100.4"}, nil, "10.150.0.79"},
		{"an entry that is not an address keeps the peer", "10.150.0.79:55000", []string{"not-an-address"}, pods, "10.150.0.79"},
		{"a trusted proxy with no header is the client", "10.150.0.79:55000", nil, pods, "10.150.0.79"},
		{"an IPv6 client", "10.150.0.79:55000", []string{"[2001:db8::1]:443"}, pods, "2001:db8::1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/v1/shares/links/x", nil)
			r.RemoteAddr = c.peer
			for _, h := range c.forwarded {
				r.Header.Add("X-Forwarded-For", h)
			}
			if got := ClientAddress(r, c.trusted); got != c.want {
				t.Errorf("ClientAddress = %q, want %q", got, c.want)
			}
			if got := RequestInfo(r, "req_1", c.trusted).IP; got != c.want {
				t.Errorf("the authorizer is told %q, want %q", got, c.want)
			}
		})
	}
}
