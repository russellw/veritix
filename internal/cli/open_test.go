package cli

import (
	"net"
	"testing"
)

// The address a listener reports is not always an address a browser can open.
// This is the whole of what --open computes, and getting it wrong produces an
// error page on the platform the flag exists for.
func TestABrowserIsSentSomewhereItCanActuallyGo(t *testing.T) {
	for _, c := range []struct {
		addr net.Addr
		want string
	}{
		{&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080}, "http://127.0.0.1:8080"},
		{&net.TCPAddr{IP: net.IPv4(192, 168, 1, 5), Port: 9000}, "http://192.168.1.5:9000"},
		// Bound to everything, which is no address at all as far as a browser
		// is concerned: the person opening it is at the machine.
		{&net.TCPAddr{IP: net.IPv4zero, Port: 8080}, "http://127.0.0.1:8080"},
		{&net.TCPAddr{IP: net.IPv6unspecified, Port: 8080}, "http://127.0.0.1:8080"},
		// A literal v6 address has to keep its brackets or the port reads as
		// part of it.
		{&net.TCPAddr{IP: net.IPv6loopback, Port: 8080}, "http://[::1]:8080"},
	} {
		if got := browseURL(c.addr); got != c.want {
			t.Errorf("listening on %s sends a browser to %s, want %s", c.addr, got, c.want)
		}
	}
}
