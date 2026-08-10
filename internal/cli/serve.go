package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/russellwallace/veritix/internal/config"
)

func newServeCmd(e *env) *cobra.Command {
	var (
		addr      string
		authToken string
		dataDir   string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the Veritix server and web interface",
		Long: "Serve starts the HTTP API and the web interface.\n\n" +
			"It binds to loopback by default. Exposing an instance to a\n" +
			"network requires both an explicit address and an auth token, so\n" +
			"that a dataset is never reachable by accident.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("addr") {
				e.cfg.Server.Addr = addr
			}
			if cmd.Flags().Changed("auth-token") {
				e.cfg.Server.AuthToken = authToken
			}
			if cmd.Flags().Changed("data-dir") {
				e.cfg.Server.DataDir = dataDir
			}

			if !config.IsLoopback(e.cfg.Server.Addr) && e.cfg.Server.AuthToken == "" {
				return fmt.Errorf(
					"refusing to listen on %s without an auth token: set --auth-token or VERITIX_AUTH_TOKEN",
					e.cfg.Server.Addr)
			}
			return runServe(e)
		},
	}

	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	f.StringVar(&authToken, "auth-token", "", "bearer token required on every API request")
	f.StringVar(&dataDir, "data-dir", "", "directory for the run store, datasets, and reports")

	return cmd
}

func runServe(_ *env) error {
	return errors.New("serve: the HTTP server is not wired up yet")
}
