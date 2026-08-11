package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/russellwallace/veritix/internal/agent"
	"github.com/russellwallace/veritix/internal/config"
	"github.com/russellwallace/veritix/internal/store"
)

// stubModel is an OpenAI-compatible endpoint that replies with a scripted
// sequence of turns.
//
// It stands in for the customer's Ollama, and it is a real HTTP server for the
// same reason the rest of this suite drives the real pipeline: the path being
// tested is request-to-report through the provider, the loop, the guard, the
// store, and back out over HTTP. Substituting a Go interface in the middle
// would leave most of that untested.
type stubModel struct {
	*httptest.Server

	mu      sync.Mutex
	replies []string
	calls   int
	// bodies is every request the server received, which is what a leak test
	// scans.
	bodies []string
}

func newStubModel(t *testing.T, replies ...string) *stubModel {
	t.Helper()
	m := &stubModel{replies: replies}

	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		m.mu.Lock()
		m.bodies = append(m.bodies, string(body))
		var reply string
		if m.calls < len(m.replies) {
			reply = m.replies[m.calls]
		} else {
			reply = `{"choices":[{"finish_reason":"stop","message":{"content":"Nothing further."}}]}`
		}
		m.calls++
		m.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *stubModel) sent() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.bodies, "\n")
}

// toolCallReply builds a chat-completions response asking for one tool call.
func toolCallReply(name string, args map[string]any) string {
	encoded, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	arguments, err := json.Marshal(string(encoded))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf(`{"model":"stub","choices":[{"finish_reason":"tool_calls","message":{
		"content":"", "tool_calls":[{"id":"c1","type":"function",
		"function":{"name":%q,"arguments":%s}}]}}],
		"usage":{"prompt_tokens":500,"completion_tokens":40}}`, name, arguments)
}

// newAgentServer builds a server configured to talk to the stub model.
func newAgentServer(t *testing.T, model *stubModel) *testServer {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.Server.DataDir = dir
	cfg.LLM.Provider = config.ProviderOpenAICompatible
	cfg.LLM.BaseURL = model.URL
	cfg.LLM.Model = "stub"
	cfg.LLM.MaxSteps = 6

	srv, err := New(t.Context(), Options{Store: st, Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return &testServer{Server: hs, t: t}
}

// The whole agentic path over HTTP: ask for a run with a model, watch it
// investigate, and read back both the finding it proved and the record of what
// it was told.
func TestAnAgenticRunProducesAFindingAndATrace(t *testing.T) {
	model := newStubModel(t,
		toolCallReply("list_tables", nil),
		toolCallReply("record_finding", map[string]any{
			"rule":     "negative_amount",
			"severity": "error",
			"table":    "orders_csv",
			"column":   "amount",
			"title":    "an order has a negative amount",
			"detail":   "a refund recorded as an order will understate revenue",
			"count_query": "SELECT count(*) FROM orders_csv " +
				"WHERE TRY_CAST(amount AS DOUBLE) < 0",
		}),
	)
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()

	run := ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})

	var doc map[string]any
	ts.decode(ts.get("/api/v1/runs/"+run.ID+"/report"), http.StatusOK, &doc)

	agentInfo, ok := doc["agent"].(map[string]any)
	if !ok {
		t.Fatalf("the report has no agent section: %v", doc["agent"])
	}
	if agentInfo["findings"].(float64) != 1 {
		t.Errorf("agent findings = %v, want 1", agentInfo["findings"])
	}
	if agentInfo["values_sent_to_model"] != false {
		t.Error("the report claims cell values were sent to the model")
	}
	if agentInfo["complete"] != true {
		t.Errorf("the run reports itself incomplete: %v", agentInfo["stopped"])
	}

	// The finding is in the report, attributed, and verified.
	var found map[string]any
	for _, f := range doc["findings"].([]any) {
		f := f.(map[string]any)
		if f["rule"] == "agent.negative_amount" {
			found = f
		}
	}
	if found == nil {
		t.Fatal("the agent's finding is not in the report")
	}
	if found["origin"] != "agent" {
		t.Errorf("origin = %v, want agent", found["origin"])
	}
	if found["verified"] != true {
		t.Error("an unverified agent finding reached the report")
	}

	// And the trace is served, carrying what actually crossed the boundary.
	var trace agent.Trace
	ts.decode(ts.get("/api/v1/runs/"+run.ID+"/trace"), http.StatusOK, &trace)

	if len(trace.Steps) != 3 {
		t.Errorf("the trace has %d steps, want 3", len(trace.Steps))
	}
	if trace.Model != "stub" {
		t.Errorf("trace model = %q", trace.Model)
	}
	if trace.Findings != 1 || trace.Refused != 0 {
		t.Errorf("trace findings = %d, refused = %d", trace.Findings, trace.Refused)
	}
	if trace.Usage.Total() == 0 {
		t.Error("the trace records no token usage")
	}
	if trace.Steps[0].Calls[0].Tool != "list_tables" {
		t.Errorf("first call = %q", trace.Steps[0].Calls[0].Tool)
	}
	if !strings.Contains(trace.Steps[0].Calls[0].Result, "orders_csv") {
		t.Errorf("the first tool result is not the table listing: %s",
			trace.Steps[0].Calls[0].Result)
	}
}

// The same guarantee as the agent package's own test, made at the boundary
// that actually carries the bytes: a real HTTP request to a real model
// endpoint.
func TestNoCellValueLeavesTheServerForTheModel(t *testing.T) {
	model := newStubModel(t,
		toolCallReply("describe_table", map[string]any{"table": "customers_csv"}),
		toolCallReply("sample_values", map[string]any{"table": "customers_csv", "column": "email"}),
		toolCallReply("run_sql", map[string]any{"query": "SELECT * FROM customers_csv"}),
	)
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()

	ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})

	sent := model.sent()
	if len(sent) < 2000 {
		t.Fatalf("only %d bytes reached the model endpoint", len(sent))
	}
	for _, raw := range []string{
		"CUS-000001", "alice@example.com", "Alice Smith", "Zürich", "Widget",
	} {
		if strings.Contains(sent, raw) {
			t.Errorf("the value %q was sent over the wire to the model", raw)
		}
	}
	if !strings.Contains(sent, "XXX-999999") {
		t.Error("no shaped value reached the model; the test may be checking nothing")
	}
}

// A run without the agent must not talk to a model at all, even with one
// configured. The decision belongs to whoever starts the run.
func TestAModelIsNotUsedUnlessTheRunAsksForOne(t *testing.T) {
	model := newStubModel(t)
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()

	run := ts.startRun(map[string]any{"dataset_id": datasetID})

	if model.sent() != "" {
		t.Error("a run that did not ask for the agent contacted the model")
	}

	var doc map[string]any
	ts.decode(ts.get("/api/v1/runs/"+run.ID+"/report"), http.StatusOK, &doc)
	if _, present := doc["agent"]; present {
		t.Error("a run with no agent produced an agent section in the report")
	}

	// Asking for the trace has to say which of the two reasons applies.
	resp := ts.get("/api/v1/runs/" + run.ID + "/trace")
	if resp.Status != http.StatusNotFound {
		t.Fatalf("trace status = %d, want 404", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "without a model") {
		t.Errorf("the 404 does not explain itself: %s", resp.Body)
	}
}

// Asking for an agent on a server that has no model configured is the
// operator's mistake, and it should fail the request they are watching.
func TestAskingForAnAgentWithNoModelConfiguredIsRefused(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()

	resp := ts.do(http.MethodPost, "/api/v1/runs",
		map[string]any{"dataset_id": datasetID, "agent": true})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "no model is configured") {
		t.Errorf("the refusal does not say what to do: %s", resp.Body)
	}
}

// A model that cannot be reached must not lose the audit: the deterministic
// findings are already proved and the run should report them.
func TestAnUnreachableModelDoesNotLoseTheAudit(t *testing.T) {
	model := newStubModel(t)
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()
	model.Close() // the endpoint goes away before the run starts

	run := ts.startRun(map[string]any{"dataset_id": datasetID, "agent": true})

	if run.Findings.Total == 0 {
		t.Error("the run reported no findings; the deterministic pass was lost")
	}

	var trace agent.Trace
	ts.decode(ts.get("/api/v1/runs/"+run.ID+"/trace"), http.StatusOK, &trace)
	if trace.Stopped != agent.StoppedProviderError {
		t.Errorf("stopped = %q, want provider_error", trace.Stopped)
	}
	if trace.Error == "" {
		t.Error("the trace does not say what went wrong")
	}
}

// The agent's progress reaches the browser through the pipeline's own log
// lines, not through a second notification mechanism bolted on beside them.
// This is what keeps the two from drifting: a stage that is logged is a stage
// the browser sees.
func TestAgentProgressArrivesOnTheEventStream(t *testing.T) {
	model := newStubModel(t,
		toolCallReply("run_sql", map[string]any{
			"query":  "SELECT count(*) FROM orders_csv",
			"reason": "how many orders are there",
		}),
	)
	ts := newAgentServer(t, model)
	datasetID := ts.registerFixture()

	var run runJSON
	ts.decode(ts.do(http.MethodPost, "/api/v1/runs",
		map[string]any{"dataset_id": datasetID, "agent": true}),
		http.StatusAccepted, &run)

	messages := ts.streamMessages(run.ID)
	joined := strings.Join(messages, "\n")

	for _, want := range []string{"agent starting", "agent query", "agent complete"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the stream never mentioned %q; it carried:\n%s", want, joined)
		}
	}
}
