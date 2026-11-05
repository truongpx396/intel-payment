package events

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
)

// ErrForbiddenAddress is a destination the service will not send to: it would turn a webhook into a way to reach the service's own
// network. It is refused before any request is sent.
var ErrForbiddenAddress = errors.New("events: destination address is not allowed")

// Refused reports whether a webhook may not be sent to addr, and why. It refuses what net/netip calls private, loopback, link-local,
// unspecified or multicast — after unwrapping an IPv4-mapped address, which would otherwise carry 127.0.0.1 past a check made on the
// IPv6 form — and the ranges a host should never be a webhook target for: the "this network" block, carrier-grade NAT, the
// documentation and benchmarking ranges, the reserved class, and the IPv6 forms that embed an IPv4 address (NAT64, 6to4).
func Refused(addr netip.Addr) error {
	if !addr.IsValid() {
		return fmt.Errorf("%w: not an address", ErrForbiddenAddress)
	}
	a := addr.Unmap().WithZone("")
	switch {
	case a.IsLoopback():
		return fmt.Errorf("%w: loopback", ErrForbiddenAddress)
	case a.IsPrivate():
		return fmt.Errorf("%w: private", ErrForbiddenAddress)
	case a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast():
		return fmt.Errorf("%w: link-local", ErrForbiddenAddress)
	case a.IsUnspecified():
		return fmt.Errorf("%w: unspecified", ErrForbiddenAddress)
	case a.IsMulticast():
		return fmt.Errorf("%w: multicast", ErrForbiddenAddress)
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return fmt.Errorf("%w: reserved (%s)", ErrForbiddenAddress, p)
		}
	}
	if a.Is6() {
		// An IPv6 address that embeds an IPv4 one is judged by the IPv4 address it carries.
		if v4, ok := embeddedV4(a); ok {
			return Refused(v4)
		}
	}
	return nil
}

var reserved = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
	"255.255.255.255/32", "2001:db8::/32", "100::/64", "::/96",
)

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

var (
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	to4   = netip.MustParsePrefix("2002::/16")
)

func embeddedV4(a netip.Addr) (netip.Addr, bool) {
	b := a.As16()
	switch {
	case nat64.Contains(a):
		return netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}), true
	case to4.Contains(a):
		return netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}), true
	}
	return netip.Addr{}, false
}

// Guard decides where a delivery may go. The zero Guard is the production one. AllowPrivate is for development only (a receiver on
// localhost); it is a deployment's explicit choice, never a default, and it still refuses nothing but the address classes it names.
type Guard struct{ AllowPrivate bool }

// CheckURL refuses a destination that can be seen to be wrong without resolving it: not `https` (`http` only for a development
// receiver on localhost, and only when AllowPrivate), carrying credentials, or naming an IP literal the guard refuses. A hostname is
// judged when it is connected to (Control), which is the only check that cannot be raced by a DNS answer that changes.
func (g Guard) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%w: %q is not a URL", ErrForbiddenAddress, raw)
	}
	if u.User != nil {
		return fmt.Errorf("%w: a URL carries no credentials", ErrForbiddenAddress)
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
	case "http":
		if !g.AllowPrivate || !isLocalHost(host) {
			return fmt.Errorf("%w: only https (http is for a development receiver on localhost)", ErrForbiddenAddress)
		}
	default:
		return fmt.Errorf("%w: scheme %q", ErrForbiddenAddress, u.Scheme)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return g.CheckAddr(ip)
	}
	return nil
}

func isLocalHost(h string) bool {
	return strings.EqualFold(h, "localhost") || h == "127.0.0.1" || h == "::1"
}

// CheckAddr is Refused, except that AllowPrivate lets loopback through for a development receiver.
func (g Guard) CheckAddr(a netip.Addr) error {
	if g.AllowPrivate && a.Unmap().IsLoopback() {
		return nil
	}
	return Refused(a)
}

// Control is a net.Dialer.Control: it runs on the address the dialer is about to connect to, after resolution, so a hostname that
// resolves to a private address — or resolves to a public one when checked and a private one when connected to — is refused before a
// byte is sent.
func (g Guard) Control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrForbiddenAddress, address)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q", ErrForbiddenAddress, host)
	}
	return g.CheckAddr(ip)
}
