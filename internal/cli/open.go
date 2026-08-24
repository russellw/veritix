package cli

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"runtime"
)

// browseURL is where to send a browser for a server listening on addr.
//
// A listener bound to 0.0.0.0 or :: is reachable at every address the machine
// has, and none of them is "0.0.0.0": handing that to a browser produces an
// error page on the one platform this feature exists for. Loopback is the one
// address that is always right for the person sitting at the machine.
func browseURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	// JoinHostPort puts the brackets back around a v6 literal.
	return "http://" + net.JoinHostPort(host, port)
}

// openBrowser asks the desktop to open url in whatever the user's browser is.
//
// This is the whole of --open, and it is a flag rather than a default because
// starting somebody else's program is not something a server should do because
// it felt helpful. It exists because the interface is the primary interface
// and its users are on Windows desktops: without it, the shortest path from a
// downloaded zip to a working screen runs through a terminal, which is the
// thing those users do not have.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "windows":
		// rundll32 rather than `cmd /c start`, which reads a quoted first
		// argument as a window title and treats & in a URL as a command
		// separator.
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	case "darwin":
		name, args = "open", []string{url}
	default:
		name, args = "xdg-open", []string{url}
	}
	// The program is a constant. The only variable is the URL, and browseURL
	// derives that from the address this process is itself listening on: a
	// host that parsed as an IP and a port that parsed as a number, or it
	// would not have been possible to listen on it. It is passed as one argv
	// element with no shell in the way.
	//
	// context.Background and not the server's context, which is the whole
	// reason this says CommandContext at all: a context-bound child is killed
	// when its context ends, so serve shutting down would close the window
	// the user is reading. A browser outlives the server that launched it.
	cmd := exec.CommandContext(context.Background(), name, args...) //nolint:gosec // the guard is the paragraph above
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not open a browser: %w", err)
	}
	// Released rather than waited for: a browser outlives the server that
	// launched it, and on Linux xdg-open exits immediately anyway.
	return cmd.Process.Release()
}
