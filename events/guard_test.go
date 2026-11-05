package events_test

import (
	"errors"
	"net/netip"
	"testing"

	"github.com/truongpx396/intel-payment/events"
)

// R115: "an endpoint resolving to a private, loopback, link-local or IPv4-mapped address is refused before any request is sent".

func TestAddressesAWebhookMustNeverReachAreRefusedAndPublicOnesAreNot(t *testing.T) {
	t.Parallel()
	refused := map[string]string{
		"10.0.0.1": "private", "172.16.5.4": "private", "192.168.1.1": "private", "fd00::1": "private (unique local)",
		"127.0.0.1": "loopback", "127.255.255.254": "loopback", "::1": "loopback",
		"169.254.169.254": "the cloud metadata address", "169.254.0.1": "link-local", "fe80::1": "link-local", "fe80::1%eth0": "link-local with a zone",
		"0.0.0.0": "unspecified", "::": "unspecified", "0.1.2.3": "this network",
		"224.0.0.1": "multicast", "239.255.255.250": "multicast", "ff02::1": "multicast", "ff0e::1": "multicast",
		"::ffff:127.0.0.1": "IPv4-mapped loopback", "::ffff:10.0.0.1": "IPv4-mapped private", "::ffff:169.254.169.254": "IPv4-mapped metadata",
		"::ffff:192.168.0.1": "IPv4-mapped private", "::ffff:0.0.0.0": "IPv4-mapped unspecified",
		"64:ff9b::7f00:1": "NAT64 carrying 127.0.0.1", "64:ff9b::a9fe:a9fe": "NAT64 carrying the metadata address", "2002:7f00:1::": "6to4 carrying 127.0.0.1",
		"2002:0a00:0001::": "6to4 carrying 10.0.0.1", "100.64.0.1": "carrier-grade NAT", "192.0.0.1": "IETF protocol assignments",
		"192.0.2.7": "documentation", "198.51.100.7": "documentation", "203.0.113.7": "documentation", "198.18.0.1": "benchmarking",
		"240.0.0.1": "reserved", "255.255.255.255": "broadcast", "2001:db8::1": "documentation", "::7f00:1": "IPv4-compatible loopback",
	}
	for s, why := range refused {
		a, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if err := events.Refused(a); !errors.Is(err, events.ErrForbiddenAddress) {
			t.Errorf("%s (%s) was allowed: %v", s, why, err)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "172.32.0.1", "100.63.255.255", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8", "64:ff9b::808:808"} {
		if err := events.Refused(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s is public and was refused: %v", s, err)
		}
	}
	if err := events.Refused(netip.Addr{}); err == nil {
		t.Error("the zero address was allowed")
	}
}

func TestADestinationIsJudgedByItsSchemeCredentialsAndLiteralAddress(t *testing.T) {
	t.Parallel()
	prod, dev := events.Guard{}, events.Guard{AllowPrivate: true}
	for _, tc := range []struct {
		url       string
		prod, dev bool // allowed?
	}{
		{"https://hooks.example.com/intel", true, true},
		{"https://hooks.example.com:8443/a/b?c=d", true, true},
		{"https://8.8.8.8/hook", true, true},
		{"http://hooks.example.com/intel", false, false},
		{"http://localhost:8080/hook", false, true},
		{"http://127.0.0.1:9000/hook", false, true},
		{"http://[::1]:9000/hook", false, true},
		{"http://10.0.0.5/hook", false, false},
		{"https://localhost/hook", true, true}, // a hostname is judged when it is connected to, not here
		{"https://127.0.0.1/hook", false, true},
		{"https://10.0.0.1/hook", false, false},
		{"https://169.254.169.254/latest/meta-data", false, false},
		{"https://[::ffff:127.0.0.1]/hook", false, true},
		{"https://[::ffff:10.0.0.1]/hook", false, false},
		{"https://[fe80::1]/hook", false, false},
		{"https://user:pass@hooks.example.com/hook", false, false},
		{"https://user@hooks.example.com/hook", false, false},
		{"ftp://hooks.example.com/hook", false, false},
		{"file:///etc/passwd", false, false},
		{"gopher://hooks.example.com", false, false},
		{"//hooks.example.com/hook", false, false},
		{"hooks.example.com", false, false},
		{"", false, false},
		{"https://", false, false},
	} {
		if err := prod.CheckURL(tc.url); (err == nil) != tc.prod {
			t.Errorf("production guard, %q: allowed=%v, want %v (%v)", tc.url, err == nil, tc.prod, err)
		}
		if err := dev.CheckURL(tc.url); (err == nil) != tc.dev {
			t.Errorf("development guard, %q: allowed=%v, want %v (%v)", tc.url, err == nil, tc.dev, err)
		}
	}
}

// The check that cannot be raced by DNS is the one made on the address being connected to.
func TestTheDialerHookRefusesTheAddressItIsAboutToConnectTo(t *testing.T) {
	t.Parallel()
	prod, dev := events.Guard{}, events.Guard{AllowPrivate: true}
	for addr, wantProd := range map[string]bool{
		"8.8.8.8:443": true, "[2606:4700:4700::1111]:443": true,
		"127.0.0.1:443": false, "10.1.2.3:443": false, "[::1]:443": false, "169.254.169.254:80": false, "[::ffff:127.0.0.1]:443": false,
		"localhost:443": false, "not-an-address": false, "": false, "8.8.8.8": false,
	} {
		if err := prod.Control("tcp", addr, nil); (err == nil) != wantProd {
			t.Errorf("production, %q: allowed=%v, want %v", addr, err == nil, wantProd)
		}
	}
	if err := dev.Control("tcp", "127.0.0.1:9000", nil); err != nil {
		t.Errorf("development loopback: %v", err)
	}
	if err := dev.Control("tcp", "10.0.0.1:9000", nil); err == nil {
		t.Error("development allowed a private address that is not loopback")
	}
	if err := dev.Control("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("development allowed the metadata address")
	}
}

// Anything net/netip calls private, loopback, link-local, unspecified or multicast — in its own form or once an IPv4-mapped address is
// unwrapped — is refused. The reference is netip's own predicates, not a table of this package's.
func FuzzNothingNetipCallsInternalIsAllowed(f *testing.F) {
	for _, s := range []string{"10.0.0.1", "127.0.0.1", "::1", "169.254.169.254", "fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", "::ffff:127.0.0.1", "8.8.8.8", "2606:4700:4700::1111"} {
		a := netip.MustParseAddr(s).As16()
		f.Add(a[:])
	}
	f.Add([]byte{10, 0, 0, 1})
	f.Fuzz(func(t *testing.T, raw []byte) {
		var a netip.Addr
		switch {
		case len(raw) == 4:
			a = netip.AddrFrom4([4]byte(raw))
		case len(raw) >= 16:
			a = netip.AddrFrom16([16]byte(raw[:16]))
		default:
			t.Skip()
		}
		u := a.Unmap()
		internal := a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
			a.IsUnspecified() || a.IsMulticast() ||
			u.IsPrivate() || u.IsLoopback() || u.IsLinkLocalUnicast() || u.IsLinkLocalMulticast() || u.IsInterfaceLocalMulticast() || u.IsUnspecified() || u.IsMulticast()
		if internal && events.Refused(a) == nil {
			t.Fatalf("%v is internal to net/netip and was allowed", a)
		}
		// A zone never makes an address acceptable.
		if events.Refused(a) != nil && events.Refused(a.WithZone("eth0")) == nil {
			t.Fatalf("%v is refused and %v%%eth0 is not", a, a)
		}
	})
}
