package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/russellwallace/veritix/internal/config"
)

// Binding to anything but loopback without a token would put a customer's data
// on the network by accident, which is the one failure this product cannot
// have. The refusal is asserted here rather than left to review.
func TestServeRefusesNetworkBindWithoutAToken(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		token   string
		refused bool
	}{
		{name: "loopback without a token", addr: "127.0.0.1:0", refused: false},
		{name: "localhost without a token", addr: "localhost:0", refused: false},
		{name: "all interfaces without a token", addr: "0.0.0.0:0", refused: true},
		{name: "a routable address without a token", addr: "192.168.1.10:0", refused: true},
		{name: "all interfaces with a token", addr: "0.0.0.0:0", token: "s3cret", refused: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := &env{cfg: config.Default()}
			cmd := newServeCmd(e)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			args := []string{"--addr", tc.addr, "--data-dir", t.TempDir()}
			if tc.token != "" {
				args = append(args, "--auth-token", tc.token)
			}
			cmd.SetArgs(args)

			// A context that is already cancelled: an accepted configuration
			// should get as far as starting a server and then stop, and a
			// refused one should never get that far at all.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			err := cmd.ExecuteContext(ctx)
			refused := err != nil && strings.Contains(err.Error(), "without an auth token")

			if refused != tc.refused {
				t.Errorf("addr %q token %q: refused = %v, want %v (error: %v)",
					tc.addr, tc.token, refused, tc.refused, err)
			}
		})
	}
}
