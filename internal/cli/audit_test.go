package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/russellwallace/veritix/internal/agent"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/telemetry"
)

const fixtureDir = "../../testdata/dirty-retail"

// rawValuesInFixture is the same list the report and agent suites use. The
// trace is a fourth place the promise has to hold — it is a file the operator
// will attach to a ticket — so it is held to it in the same terms.
var rawValuesInFixture = []string{
	"CUS-000001", "CUS-000005", "CUS-999999",
	"alice@example.com", "carol@example.com",
	"Alice Smith", "Frank Green",
	"Zürich", "München", "Montréal",
	"Doohickey", "Widget",
	"Quarterly Sales Report",
}

// stubChatModel is an OpenAI-compatible endpoint replying with scripted turns,
// so the CLI's agent path can be driven without a model. It is deliberately the
// same idea as internal/api's stubModel: what is under test is Veritix's
// handling of what a model said.
func stubChatModel(t *testing.T, replies ...string) string {
	t.Helper()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		reply := `{"choices":[{"finish_reason":"stop","message":{"content":"Nothing further."}}]}`
		if calls < len(replies) {
			reply = replies[calls]
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1"
}

func newEnv() *env {
	return &env{
		cfg: config.Default(),
		log: telemetry.NewLogger(io.Discard, "error", "text"),
	}
}

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newAuditCmd(newEnv())
	var out, errBuf bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	return out.String(), cmd.ExecuteContext(context.Background())
}

// Both refusals happen before the audit does, because an audit is minutes of
// work and a flag that cannot be honoured should not cost them.
func TestTraceOutRefusesWhatItCannotDo(t *testing.T) {
	t.Run("no model to trace", func(t *testing.T) {
		// The dataset path does not exist either. The trace refusal is the error
		// that comes back only if it is reached first, which is what "before the
		// audit" has to mean to be worth anything.
		_, err := runCmd(t, filepath.Join(t.TempDir(), "no-such-dataset"),
			"--trace-out", filepath.Join(t.TempDir(), "trace.json"))
		if err == nil {
			t.Fatal("asking for a trace with no model configured was accepted")
		}
		// The message has to name the fix. "No trace was produced" would leave
		// the operator wondering whether the model simply did nothing.
		if !strings.Contains(err.Error(), "--llm") {
			t.Errorf("the flags were not checked before the dataset was opened: %v", err)
		}
	})

	t.Run("two documents on one stdout", func(t *testing.T) {
		_, err := runCmd(t, fixtureDir,
			"--llm", "openai-compatible",
			"--llm-base-url", stubChatModel(t),
			"--llm-model", "stub",
			"--trace-out", "-",
			"--output", "-")
		if err == nil {
			t.Fatal("writing the report and the trace to stdout together was accepted")
		}
		if !strings.Contains(err.Error(), "stdout") {
			t.Errorf("the refusal does not explain the collision: %v", err)
		}
	})
}

// The trace is the CLI's answer to "what did the model see". Until this flag it
// was reachable only over HTTP, which left the entry point used for debugging a
// model as the one that could not show what the model was sent.
func TestTraceOutRecordsWhatTheModelWasSent(t *testing.T) {
	dir := t.TempDir()
	tracePath := filepath.Join(dir, "trace.json")

	base := stubChatModel(t,
		`{"choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[`+
			`{"id":"c1","type":"function","function":{"name":"list_tables","arguments":"{}"}}]}}],`+
			`"usage":{"prompt_tokens":100,"completion_tokens":10}}`,
		`{"choices":[{"finish_reason":"stop","message":{"content":"Nothing further."}}]}`,
	)

	if _, err := runCmd(t, fixtureDir,
		"--llm", "openai-compatible",
		"--llm-base-url", base,
		"--llm-model", "stub",
		"--output", filepath.Join(dir, "report.txt"),
		"--trace-out", tracePath,
	); err != nil {
		t.Fatalf("audit: %v", err)
	}

	raw, err := os.ReadFile(tracePath) //nolint:gosec // a path this test just made
	if err != nil {
		t.Fatalf("reading the trace: %v", err)
	}

	// It has to be the same document the API serves, or the CLI has invented a
	// second answer to the same question.
	var tr agent.Trace
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("the trace is not the document /runs/{id}/trace serves: %v", err)
	}
	if tr.Model != "stub" || tr.Provider != "openai-compatible" {
		t.Errorf("trace records provider %q model %q", tr.Provider, tr.Model)
	}
	if len(tr.Steps) == 0 {
		t.Fatal("the trace records no steps")
	}
	if tr.Steps[0].Calls[0].Tool != "list_tables" {
		t.Errorf("first call recorded as %q, want list_tables", tr.Steps[0].Calls[0].Tool)
	}
	if tr.ValuesAllowed {
		t.Error("the trace claims values were permitted when they were not")
	}

	for _, value := range rawValuesInFixture {
		if bytes.Contains(raw, []byte(value)) {
			t.Errorf("the trace file leaks the raw value %q", value)
		}
	}
}

// A trace is only worth writing where it can be written, and finding that out
// after the audit rather than before it is the whole point of checking early.
func TestTraceOutRejectsAnUnwritablePathBeforeAuditing(t *testing.T) {
	_, err := runCmd(t, fixtureDir,
		"--llm", "openai-compatible",
		"--llm-base-url", stubChatModel(t),
		"--llm-model", "stub",
		"--trace-out", filepath.Join(t.TempDir(), "no-such-dir", "trace.json"))
	if err == nil {
		t.Fatal("a trace path in a directory that does not exist was accepted")
	}
	if !strings.Contains(err.Error(), "trace file") {
		t.Errorf("the error does not say what could not be created: %v", err)
	}
}
