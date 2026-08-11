package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/russellwallace/veritix/internal/api"
	"github.com/russellwallace/veritix/internal/buildinfo"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/store"
	"github.com/russellwallace/veritix/web"
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
			return runServe(cmd.Context(), e)
		},
	}

	f := cmd.Flags()
	f.StringVar(&addr, "addr", "127.0.0.1:8080", "listen address")
	f.StringVar(&authToken, "auth-token", "", "bearer token required on every API request")
	f.StringVar(&dataDir, "data-dir", "", "directory for the run store, datasets, and reports")

	return cmd
}

func runServe(ctx context.Context, e *env) error {
	// The data directory holds the run store, uploaded datasets, and the
	// DuckDB file each run leaves behind. 0700 because all three are customer
	// data on a machine that may have other users.
	dir := e.cfg.Server.DataDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("could not create the data directory %s: %w", dir, err)
	}

	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		return err
	}
	defer st.Close() //nolint:errcheck // the shutdown error below is the one worth reporting

	srv, err := api.New(ctx, api.Options{
		Store:   st,
		Config:  e.cfg,
		Version: buildinfo.Short(),
		Log:     e.log,
		Web:     web.FS(),
	})
	if err != nil {
		return err
	}
	if !web.Built() {
		// Worth saying out loud: the API works, so this looks like a working
		// server right up until somebody opens it in a browser.
		e.log.Warn("this binary has no web interface built into it; run `make web` and rebuild")
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		// An audit streams progress for as long as it runs, so there is no
		// write timeout to impose: a slow response here is the product
		// working. ReadHeaderTimeout is what closes a connection that opens
		// and then says nothing.
		ReadHeaderTimeout: e.cfg.Server.ReadHeaderTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	// Listen before announcing, so that a port already in use is an error the
	// operator sees instead of a URL that does not work.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", e.cfg.Server.Addr)
	if err != nil {
		return fmt.Errorf("could not listen on %s: %w", e.cfg.Server.Addr, err)
	}

	e.log.Info("veritix is serving",
		"addr", ln.Addr().String(),
		"data_dir", dir,
		"auth", e.cfg.Server.AuthToken != "")
	fmt.Fprintf(os.Stderr, "\n  Veritix is running at http://%s\n  Press Ctrl-C to stop.\n\n", ln.Addr())

	// Ctrl-C and the signal a container runtime sends both mean the same
	// thing: finish what you can, then stop.
	sigCtx, stopSignals := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	errs := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-sigCtx.Done():
	}

	e.log.Info("shutting down")

	// The API is closed before the HTTP server, not after: an event stream
	// stays open by design, so Shutdown would sit and wait out its whole
	// timeout on connections that are behaving correctly. Closing the API
	// cancels the runs and ends their streams, and then the drain is quick.
	if err := srv.Close(); err != nil {
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cfg.Server.ShutdownTimeout)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		e.log.Warn("shutdown timed out with connections still open",
			"timeout", e.cfg.Server.ShutdownTimeout)
	}
	return nil
}
