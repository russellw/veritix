package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/agent/llm/llmtest"
	"github.com/russellw/veritix/internal/config"
)

// TestMain doubles as a context server when the environment says so.
//
// The whole point of testing this here rather than in internal/mcpclient is
// that audit.Run starts a subprocess from a config file's worth of strings and
// closes it again, and nothing an in-process transport can do exercises that.
// So the test binary is the customer's MCP server.
func TestMain(m *testing.M) {
	if dir := os.Getenv("VERITIX_TEST_SERVE_DIR"); dir != "" {
		serveContext(dir)
		return
	}
	os.Exit(m.Run())
}

func serveContext(dir string) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "docs", Version: "v1"}, nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		os.Exit(1)
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		srv.AddResource(&sdk.Resource{
			URI:  "file://" + path,
			Name: strings.TrimSuffix(e.Name(), ".md"),
		}, func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			data, err := os.ReadFile(strings.TrimPrefix(req.Params.URI, "file://"))
			if err != nil {
				return nil, err
			}
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
				URI: req.Params.URI, Text: string(data),
			}}}, nil
		})
	}
	if err := srv.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		os.Exit(1)
	}
}

// The pipeline connects the context servers, hands the catalog to the agent,
// and shuts them down again — which is the argument internal/runs makes one
// layer up, applied here: four entry points each remembering to connect and to
// close is how one of them eventually forgets.
func TestThePipelineConnectsTheContextServersForTheAgent(t *testing.T) {
	script := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{{
			Name: "read_context", Input: map[string]any{"id": "data-dictionary"},
		}}},
		llmtest.Turn{Text: "Read the dictionary."},
	)

	res, err := Run(t.Context(), Options{
		Paths:   []string{"../../testdata/dirty-meters"},
		Engine:  config.Default().Engine,
		Agent:   &agent.Options{Provider: script, MaxSteps: 4},
		Context: contextConfig(t),
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	ct := res.Trace.Context
	if ct == nil {
		t.Fatal("the run recorded no context, so the servers were never connected")
	}
	if len(ct.Documents) != 3 {
		t.Errorf("the catalog has %d documents, want the fixture's 3", len(ct.Documents))
	}
	if ct.Read != 1 {
		t.Errorf("the model read %d documents, want 1", ct.Read)
	}

	call := res.Trace.Steps[0].Calls[0]
	if call.IsError {
		t.Fatalf("reading the dictionary failed: %s", call.Result)
	}
	if !strings.Contains(call.Result, "odometer") {
		t.Errorf("the dictionary did not arrive intact: %s", call.Result)
	}
}

// Configuring nothing changes nothing, which is what makes the unaided half of
// a scorecard a control rather than a second experiment.
func TestWithNoContextConfiguredTheAgentIsOfferedNone(t *testing.T) {
	script := llmtest.New(llmtest.Turn{Calls: []llmtest.Call{{
		Name: "read_context", Input: map[string]any{"id": "data-dictionary"},
	}}})

	res, err := Run(t.Context(), Options{
		Paths:  []string{"../../testdata/dirty-meters"},
		Engine: config.Default().Engine,
		Agent:  &agent.Options{Provider: script, MaxSteps: 4},
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	if res.Trace.Context != nil {
		t.Error("a run with no context server recorded one")
	}
	if call := res.Trace.Steps[0].Calls[0]; !call.IsError {
		t.Error("read_context was offered to a run that has no context server")
	}
}

// A context server that is down is a warning. An audit that died because a
// data dictionary was unreachable would be worse than one that runs unaided —
// and the unaided targets are exactly what still gets scored.
func TestAContextServerThatWillNotStartDoesNotFailTheAudit(t *testing.T) {
	script := llmtest.New(llmtest.Turn{Text: "Nothing to do."})

	res, err := Run(t.Context(), Options{
		Paths:  []string{"../../testdata/dirty-meters"},
		Engine: config.Default().Engine,
		Agent:  &agent.Options{Provider: script, MaxSteps: 2},
		Context: config.Context{Servers: []config.ContextServer{{
			Name:    "dictionary",
			Command: filepath.Join(t.TempDir(), "no-such-program"),
		}}},
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run failed rather than running unaided: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })

	if res.Findings.Len() == 0 {
		t.Error("the deterministic pass produced nothing, so nothing was audited")
	}
	servers := res.Trace.Context.Servers
	if len(servers) != 1 || servers[0].Error == "" {
		t.Errorf("the trace does not say why the server contributed nothing: %+v", servers)
	}
}

// contextConfig points a run at the fixture's documents, served by this test
// binary.
func contextConfig(t *testing.T) config.Context {
	t.Helper()
	dir, err := filepath.Abs("../../testdata/dirty-meters/context")
	if err != nil {
		t.Fatal(err)
	}
	return config.Context{Servers: []config.ContextServer{{
		Name:    "docs",
		Command: os.Args[0],
		Env:     []string{"VERITIX_TEST_SERVE_DIR=" + dir},
	}}}
}
