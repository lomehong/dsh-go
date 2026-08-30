package webfetchhttp

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strings"
	"time"

	"dshgo/web"
)

// PublicAddress is one address resolved and retained for the subsequent
// pinned connection.
type PublicAddress struct {
	// Canonical textual IPv4 or IPv6 address.
	Address string
	// Address family: 4 or 6.
	Family int
}

// AddressResolver resolves a hostname once; implementations must reject
// non-public destinations before returning. Overridden only by focused
// tests (the official resolver seam).
type AddressResolver func(ctx context.Context, hostname string) ([]PublicAddress, error)

// RFC 6052 prefix lengths that may carry an IPv4 destination through NAT64.
var rfc6052PrefixLengths = []int{32, 40, 48, 56, 64, 96}

const ipv4onlyDiscoveryHost = "ipv4only.arpa"

var ipv4onlySentinels = map[string]bool{"192.0.0.170": true, "192.0.0.171": true}

type nat64Prefix struct {
	bytes  []byte
	length int
}

// isPublicUnicast returns whether an address is globally reachable unicast.
// IPv4-mapped IPv6 is classified by its embedded IPv4 address; transition
// and translation prefixes remain blocked because their eventual IPv4
// destination cannot be pinned here (the official ipaddr.js "unicast"
// range check).
func isPublicUnicast(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if mapped := ip.To4(); mapped != nil && len(ip) == net.IPv6len && ip.IsPrivate() {
		// IPv4-mapped IPv6: classify by the embedded IPv4 address below.
		ip = mapped
	} else if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip.To4() == nil {
		// IPv6-only exclusions beyond the net.IP helpers.
		if ip[0]&0xfe == 0xfc { // fc00::/7 unique local
			return false
		}
		if len(ip) == net.IPv6len && ip[0] == 0x20 && ip[1] == 0x01 &&
			ip[2] == 0x0d && ip[3] == 0xb8 { // 2001:db8::/32 documentation
			return false
		}
	} else {
		// IPv4 exclusions the official ipaddr.js unicast range rejects.
		if ip[0] == 0 || ip[0] == 100 && ip[1]&0xc0 == 0x40 || // 0.0.0.0/8, 100.64.0.0/10 CGNAT
			ip[0] == 192 && ip[1] == 0 && ip[2] == 0 || // 192.0.0.0/24 IETF protocol assignments
			ip[0] == 198 && ip[1]&0xfe == 0x12 || // 198.18.0.0/15 benchmarking
			ip[0] >= 240 { // 240.0.0.0/4 reserved
			return false
		}
	}
	return true
}

// stripIPv6Brackets removes the WHATWG brackets around IPv6 hostnames; IP
// parsers do not accept them.
func stripIPv6Brackets(hostname string) string {
	if strings.HasPrefix(hostname, "[") && strings.HasSuffix(hostname, "]") {
		return hostname[1 : len(hostname)-1]
	}
	return hostname
}

// defaultResolver performs system DNS resolution, keeping answer order.
func defaultResolver(ctx context.Context, hostname string) ([]PublicAddress, error) {
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, hostname)
	if err != nil {
		return nil, err
	}
	addresses := make([]PublicAddress, 0, len(ips))
	for _, entry := range ips {
		family := 4
		if entry.IP.To4() == nil {
			family = 6
		}
		addresses = append(addresses, PublicAddress{Address: entry.IP.String(), Family: family})
	}
	return addresses, nil
}

// resolvePublicAddresses resolves a hostname once and rejects the complete
// answer set if any destination is not public. The returned addresses are
// the only ones the transport may use. The hostname arrives bracketed when
// it is an IPv6 literal.
func resolvePublicAddresses(ctx context.Context, hostname string) ([]PublicAddress, error) {
	return resolveWith(ctx, hostname, defaultResolver)
}

func resolveWith(ctx context.Context, hostname string, resolver AddressResolver) ([]PublicAddress, error) {
	unbracketed := stripIPv6Brackets(hostname)
	var resolved []PublicAddress
	if literal := net.ParseIP(unbracketed); literal != nil {
		family := 4
		if literal.To4() == nil {
			family = 6
		}
		resolved = []PublicAddress{{Address: unbracketed, Family: family}}
	} else {
		var err error
		resolved, err = resolver(ctx, unbracketed)
		if err != nil {
			return nil, err
		}
	}

	if len(resolved) == 0 {
		return nil, web.NewWebError("WEB_PROVIDER_ERROR",
			`hostname "`+hostname+`" resolved to no addresses`, nil)
	}

	hasIPv6 := false
	for _, entry := range resolved {
		if entry.Family == 6 && net.ParseIP(entry.Address) != nil && net.ParseIP(entry.Address).To4() == nil {
			hasIPv6 = true
		}
	}
	var nat64Prefixes []nat64Prefix
	if hasIPv6 {
		prefixes, err := discoverNat64Prefixes(ctx, resolver)
		if err != nil {
			return nil, err
		}
		nat64Prefixes = prefixes
	}

	addresses := make([]PublicAddress, 0, len(resolved))
	for _, entry := range resolved {
		ip := net.ParseIP(entry.Address)
		if (entry.Family != 4 && entry.Family != 6) || ip == nil ||
			(entry.Family == 4 && ip.To4() == nil) || (entry.Family == 6 && ip.To4() != nil && len(ip) == net.IPv4len) {
			return nil, web.NewWebError("WEB_PROVIDER_ERROR",
				`hostname "`+hostname+`" resolved to an invalid IP address`, nil)
		}
		if !isPublicUnicast(ip) {
			return nil, web.NewWebError("WEB_BLOCKED_URL",
				`URL hostname "`+hostname+`" resolves to a non-public IP address`, nil)
		}
		if translated := translatedIPv4Address(ip, nat64Prefixes); translated != "" &&
			!isPublicUnicast(net.ParseIP(translated)) {
			return nil, web.NewWebError("WEB_BLOCKED_URL",
				`URL hostname "`+hostname+`" resolves through NAT64 to a non-public IPv4 address`, nil)
		}
		addresses = append(addresses, entry)
	}
	return addresses, nil
}

// discoverNat64Prefixes discovers the active DNS64 prefix set using RFC
// 7050's reserved hostname.
func discoverNat64Prefixes(ctx context.Context, resolver AddressResolver) ([]nat64Prefix, error) {
	discovered, err := resolver(ctx, ipv4onlyDiscoveryHost)
	if err != nil {
		return nil, err
	}
	prefixes := []nat64Prefix{}
	seen := map[string]bool{}
	for _, entry := range discovered {
		ip := net.ParseIP(entry.Address)
		if entry.Family != 6 || ip == nil || ip.To4() != nil && len(ip) == net.IPv4len {
			continue
		}
		v6 := ip.To16()
		if v6 == nil {
			continue
		}
		for _, length := range rfc6052PrefixLengths {
			embedded := embeddedIPv4Address(v6, length)
			if embedded == "" || !ipv4onlySentinels[embedded] {
				continue
			}
			prefixBytes := append([]byte(nil), v6[:length/8]...)
			key := string(rune('0'+length/10)) + string(rune('0'+length%10)) + ":" + net.IP(prefixBytes).String()
			if seen[key] {
				continue
			}
			seen[key] = true
			prefixes = append(prefixes, nat64Prefix{bytes: prefixBytes, length: length})
		}
	}
	return prefixes, nil
}

// translatedIPv4Address returns the RFC 6052-embedded IPv4 address when an
// IPv6 address matches a discovered prefix.
func translatedIPv4Address(ip net.IP, prefixes []nat64Prefix) string {
	v6 := ip.To16()
	if v6 == nil || ip.To4() != nil && len(ip) == net.IPv4len {
		return ""
	}
	for _, prefix := range prefixes {
		match := true
		for index, b := range prefix.bytes {
			if v6[index] != b {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if embedded := embeddedIPv4Address(v6, prefix.length); embedded != "" {
			return embedded
		}
	}
	return ""
}

// embeddedIPv4Address extracts one IPv4 address from an RFC 6052 IPv6
// layout.
func embeddedIPv4Address(bytes net.IP, prefixLength int) string {
	if prefixLength == 96 {
		return bytes[12:16].String()
	}
	if bytes[8] != 0 {
		return ""
	}
	prefixBytes := prefixLength / 8
	beforeReservedOctet := 8 - prefixBytes
	ipv4 := net.IPv4(0, 0, 0, 0).To4()
	copy(ipv4[:beforeReservedOctet], bytes[prefixBytes:prefixBytes+beforeReservedOctet])
	copy(ipv4[beforeReservedOctet:], bytes[9:9+4-beforeReservedOctet])
	return ipv4.String()
}

// pinnedTransport builds the HTTP transport whose dialer connects only to
// the already validated address set. The URL hostname remains intact for the
// HTTP Host header and TLS SNI.
func pinnedTransport(addresses []PublicAddress, hostname string) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].Address, port))
		},
		TLSClientConfig:     &tls.Config{ServerName: hostname},
		MaxIdleConnsPerHost: 1,
	}
}

// requestPinned performs one GET through the pinned transport; redirects are
// surfaced to the caller (redirect: manual), and the response body stays
// readable until Close is called.
func requestPinned(ctx context.Context, u *http.Request, addresses []PublicAddress) (*http.Response, func(), error) {
	transport := pinnedTransport(addresses, u.URL.Hostname())
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(u)
	closer := func() { transport.CloseIdleConnections() }
	if err != nil {
		closer()
		return nil, nil, err
	}
	return resp, closer, nil
}
