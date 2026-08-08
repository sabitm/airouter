package media

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ParseHTTPURL validates that raw is an http(s) URL with no userinfo.
func ParseHTTPURL(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, ErrUnsafeURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsafeURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fmt.Errorf("%w: scheme %q", ErrUnsafeURL, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: userinfo not allowed", ErrUnsafeURL)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("%w: missing host", ErrUnsafeURL)
	}
	// Reject literal unsafe IPs in the host before DNS.
	if ip := net.ParseIP(host); ip != nil {
		if unsafeIP(ip) {
			return nil, fmt.Errorf("%w: address %s not allowed", ErrUnsafeURL, ip)
		}
	}
	return u, nil
}

// unsafeIP reports whether ip is loopback, private, link-local, unspecified,
// multicast, CGNAT, documentation, or otherwise non-public.
func unsafeIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// IPv4-mapped or plain IPv4 special ranges beyond IsPrivate.
	if v4 := ip.To4(); v4 != nil {
		// 0.0.0.0/8
		if v4[0] == 0 {
			return true
		}
		// 100.64.0.0/10 CGNAT
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// 169.254.0.0/16 link-local (also IsLinkLocalUnicast)
		if v4[0] == 169 && v4[1] == 254 {
			return true
		}
		// 192.0.0.0/24 IETF protocol assignments
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
			return true
		}
		// 192.0.2.0/24, 198.51.100.0/24, 203.0.113.0/24 documentation
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 2 {
			return true
		}
		if v4[0] == 198 && v4[1] == 51 && v4[2] == 100 {
			return true
		}
		if v4[0] == 203 && v4[1] == 0 && v4[2] == 113 {
			return true
		}
		// 198.18.0.0/15 benchmarking
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		// 240.0.0.0/4 reserved
		if v4[0] >= 240 {
			return true
		}
		return false
	}
	// IPv6: unique local fc00::/7, documentation 2001:db8::/32, IPv4-mapped handled above.
	if ip[0] == 0xfc || ip[0] == 0xfd {
		return true
	}
	// 2001:db8::/32
	if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8 {
		return true
	}
	// 2001:2::/48 benchmarking
	if ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x00 && ip[3] == 0x02 {
		return true
	}
	return false
}

// AllIPsSafe returns nil only when every address is public/safe. Any unsafe
// address fails closed (including mixed public+private DNS results).
func AllIPsSafe(addrs []net.IP) error {
	if len(addrs) == 0 {
		return fmt.Errorf("%w: no addresses", ErrUnsafeURL)
	}
	for _, ip := range addrs {
		if unsafeIP(ip) {
			return fmt.Errorf("%w: address %s not allowed", ErrUnsafeURL, ip)
		}
	}
	return nil
}

// PickDialIP selects the first safe address for dialing after AllIPsSafe.
func PickDialIP(addrs []net.IP) net.IP {
	for _, ip := range addrs {
		if !unsafeIP(ip) {
			return ip
		}
	}
	return nil
}
