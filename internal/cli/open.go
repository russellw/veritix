package cli

import (
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
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 rather than `cmd /c start`, which reads a quoted first
		// argument as a window title and treats & in a URL as a command
		// separator.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("could not open a browser: %w", err)
	}
	// Released rather than waited for: a browser outlives the server that
	// launched it, and on Linux xdg-open exits immediately anyway.
	return cmd.Process.Release()
}
