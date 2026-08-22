package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/russellw/veritix/internal/config"
)

// The context flags are shared by `audit` and `eval`, which are the two
// commands where somebody drives one run by hand and wants to see what the
// customer's own documents bought. `serve` and `mcp` take their servers from
// the configuration file alone, because they are long-lived processes an
// operator configured rather than commands somebody is standing over.

// contextFlags are the flags a command takes for the customer's own documents.
type contextFlags struct {
	servers   []string
	noContext bool
}

func addContextFlags(cmd *cobra.Command, f *contextFlags) {
	cmd.Flags().StringArrayVar(&f.servers, "context-server", nil,
		"an MCP server holding the customer's own documents, as name:command with "+
			"space-separated arguments; repeatable. For a command with a space in it, "+
			"configure context.servers in the config file instead")
	cmd.Flags().BoolVar(&f.noContext, "no-context", false,
		"ignore any configured context servers, which is how to measure what they bought")
}

// resolveContext applies the flags to the configured servers.
//
// --no-context wins over everything, including a server named on the command
// line, because its whole job is to produce the unaided run that the aided one
// is compared against: a flag that could be quietly overridden by another flag
// would make that comparison a coin toss.
func resolveContext(cfg config.Context, f contextFlags) (config.Context, error) {
	if f.noContext {
		cfg.Servers = nil
		return cfg, nil
	}
	for _, spec := range f.servers {
		srv, err := parseContextServer(spec)
		if err != nil {
			return cfg, err
		}
		cfg.Servers = append(cfg.Servers, srv)
	}
	return cfg, nil
}

// parseContextServer reads one --context-server value.
//
// The format is deliberately crude — a name, a colon, and a command line split
// on whitespace — because the flag exists to drive a run by hand and to
// demonstrate the thing, not to be the configuration surface. Anything a
// crude parser gets wrong is a reason to write the server down in the config
// file, where the arguments are a list and nothing has to be split at all.
func parseContextServer(spec string) (config.ContextServer, error) {
	name, rest, ok := strings.Cut(spec, ":")
	name, rest = strings.TrimSpace(name), strings.TrimSpace(rest)
	if !ok || name == "" || rest == "" {
		return config.ContextServer{}, fmt.Errorf(
			"--context-server %q: want name:command, as in docs:/usr/local/bin/docs-mcp", spec)
	}
	fields := strings.Fields(rest)
	return config.ContextServer{Name: name, Command: fields[0], Args: fields[1:]}, nil
}
