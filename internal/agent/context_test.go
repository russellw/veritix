package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/llm/llmtest"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/engine"
	"github.com/russellw/veritix/internal/ingest"
	"github.com/russellw/veritix/internal/mcpclient"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/source"
)

// These tests run against dirty-meters, which is the fixture built for this:
// four of its six agent targets are invisible in the export and become visible
// only when the customer's own documents are read. The documents are served
// over a real MCP connection rather than read off disk, because reading them
// off disk would test a feature that does not exist.

const metersDir = "../../testdata/dirty-meters"

// valuesInMeters are verbatim contents of the dirty-meters files that appear in
// none of its context documents.
//
// The qualification is the whole point. A data dictionary explains the join
// with "UPN-4471 is premises 4471" and names the permitted statuses, so those
// really are cell values and they really do reach the model — admitted
// deliberately, by an operator who configured a server, and counted by
// redact.Guard.Document. What must still not leak is everything else, and this
// is the list that separates the two.
var valuesInMeters = []string{
	"MTR-0009", "RDG-100025",
	"12 Alder Road", "Kestrel", "BS1 4TH", "EH9 1HZ",
	"Standard Domestic A",
	"dormant", "pending_install", "decommissioned",
}

// metersFixture loads dirty-meters and connects a library to its context
// directory, served by a real MCP server in this process.
func metersFixture(t *testing.T, withContext bool) Input {
	t.Helper()
	ctx := t.Context()

	e, err := engine.Open(ctx, "", config.Default().Engine, nil)
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	ds, err := source.Discover([]string{metersDir})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	loaded, err := ingest.Load(ctx, e, ds, ingest.Options{}, nil)
	if err != nil {
		t.Fatalf("ingest.Load: %v", err)
	}
	prof, err := profile.Run(ctx, e, loaded, profile.Options{}, nil)
	if err != nil {
		t.Fatalf("profile.Run: %v", err)
	}
	if err := e.Lockdown(ctx); err != nil {
		t.Fatalf("Lockdown: %v", err)
	}

	in := Input{Engine: e, Profile: prof, Root: ds.Root}
	if withContext {
		in.Context = contextLibrary(t)
	}
	return in
}

// contextLibrary serves testdata/dirty-meters/context over MCP.
func contextLibrary(t *testing.T) *mcpclient.Library {
	t.Helper()

	dir := filepath.Join(metersDir, "context")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the context directory: %v", err)
	}

	srv := sdk.NewServer(&sdk.Implementation{Name: "docs", Version: "v1"}, nil)
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		srv.AddResource(&sdk.Resource{
			URI:         "veritix-test://" + entry.Name(),
			Name:        strings.TrimSuffix(entry.Name(), ".md"),
			Description: "the customer's " + strings.TrimSuffix(entry.Name(), ".md"),
			MIMEType:    "text/markdown",
		}, func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			data, err := os.ReadFile(path) //nolint:gosec // a fixture path
			if err != nil {
				return nil, err
			}
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
				URI: req.Params.URI, Text: string(data),
			}}}, nil
		})
	}

	serverT, clientT := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })

	lib, err := mcpclient.Connect(t.Context(), mcpclient.Options{
		Servers: []mcpclient.Server{{Name: "docs", Transport: clientT}},
	})
	if err != nil {
		t.Fatalf("mcpclient.Connect: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib
}

func metersOptions(p llm.Provider) Options {
	return Options{Provider: p, MaxSteps: 8, MaxRows: 20}
}

// This is M5b in one test: a defect nothing in the export marks out, found
// because the customer's own dictionary says what the column may contain.
//
// The model reads the dictionary, writes the query the dictionary implies, and
// records what the engine measures. The count is the manifest's — three meters
// in states the dictionary does not permit — and it is the engine that produced
// it, not the model.
func TestADocumentMakesADefectVisibleThatTheDataDoesNot(t *testing.T) {
	in := metersFixture(t, true)

	provider := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{{
			ID: "1", Name: "read_context", Input: map[string]any{"id": "data-dictionary"},
		}}},
		llmtest.Turn{Calls: []llmtest.Call{{
			ID: "2", Name: "record_finding", Input: map[string]any{
				"rule":  "meters.undocumented_status",
				"title": "3 meters are in a lifecycle state the data dictionary does not permit",
				"detail": "The dictionary permits exactly active, inactive and removed. " +
					"Every downstream report filters this column by name, so these meters " +
					"are silently missing from billing and from the regulatory return.",
				"severity":       "error",
				"table":          "meters_csv",
				"column":         "status",
				"affected_count": 3,
				"count_query": "SELECT count(*) FROM meters_csv " +
					"WHERE status NOT IN ('active', 'inactive', 'removed')",
				"remedy": "map each state onto a permitted one, or extend the dictionary",
			},
		}}},
		llmtest.Turn{Text: "The status column disagrees with the dictionary."},
	)

	res, err := Run(t.Context(), in, metersOptions(provider), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("recorded %d findings, want 1: %+v", len(res.Findings), res.Findings)
	}
	f := res.Findings[0]
	if f.Count != 3 {
		t.Errorf("the engine measured %d, want the manifest's 3", f.Count)
	}
	if f.Location.Column != "status" {
		t.Errorf("the finding is at %+v", f.Location)
	}
}

// The document has to arrive as the customer wrote it. A dictionary rendered as
// shapes explains nothing, which is why Guard.Document exists — and why it
// counts what it admitted rather than leaving a reader to add it up.
func TestADocumentReachesTheModelVerbatimAndIsCounted(t *testing.T) {
	in := metersFixture(t, true)

	provider := llmtest.New(llmtest.Turn{Calls: []llmtest.Call{{
		ID: "1", Name: "read_context", Input: map[string]any{"id": "data-dictionary"},
	}}})

	res, err := Run(t.Context(), in, metersOptions(provider), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The sentence four of the six aided targets turn on.
	const sentence = "It is the odometer, not the trip"
	var found bool
	for _, step := range res.Trace.Steps {
		for _, call := range step.Calls {
			if call.Tool == "read_context" && strings.Contains(call.Result, sentence) {
				found = true
			}
		}
	}
	if !found {
		t.Error("the dictionary did not reach the model intact")
	}
	if res.Trace.Redaction.Documents != 1 {
		t.Errorf("the guard counted %d documents admitted, want 1", res.Trace.Redaction.Documents)
	}
	if res.Trace.Redaction.DocumentBytes < 1000 {
		t.Errorf("the guard counted %d bytes admitted, which is too few for the dictionary",
			res.Trace.Redaction.DocumentBytes)
	}
}

// Admitting documents does not admit the data. Everything the guard did before
// M5b it still does, and the one exception is the documents themselves — which
// is why valuesInMeters is the list of fixture values no document mentions.
func TestARunWithContextStillSendsNoCellValue(t *testing.T) {
	in := metersFixture(t, true)

	// A model that reads everything it is offered and then queries the widest
	// surface the guard has to hold.
	var script []llmtest.Turn
	for i, d := range in.Context.Catalog() {
		script = append(script, llmtest.Turn{Calls: []llmtest.Call{{
			ID: string(rune('a' + i)), Name: "read_context", Input: map[string]any{"id": d.ID},
		}}})
	}
	script = append(script, llmtest.Turn{Calls: []llmtest.Call{
		{ID: "q", Name: "run_sql", Input: map[string]any{
			"sql": "SELECT * FROM premises_csv",
		}},
		{ID: "s", Name: "sample_values", Input: map[string]any{
			"table": "meters_csv", "column": "status",
		}},
	}})

	provider := llmtest.New(script...)
	if _, err := Run(t.Context(), in, metersOptions(provider), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for i, req := range provider.Requests() {
		body, err := json.Marshal(req)
		if err != nil {
			t.Fatalf("marshal request %d: %v", i, err)
		}
		for _, v := range valuesInMeters {
			if strings.Contains(string(body), v) {
				t.Errorf("request %d carries the cell value %q", i, v)
			}
		}
	}
}

// A model will invent an id. The correction names the real ones, because the
// catalog is short and the list is the thing that actually helps.
func TestAnInventedDocumentIDIsCorrectedRatherThanFetched(t *testing.T) {
	in := metersFixture(t, true)

	provider := llmtest.New(llmtest.Turn{Calls: []llmtest.Call{{
		ID: "1", Name: "read_context", Input: map[string]any{"id": "the-data-dictionary"},
	}}})

	res, err := Run(t.Context(), in, metersOptions(provider), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	call := res.Trace.Steps[0].Calls[0]
	if !call.IsError {
		t.Fatal("an id that is not in the catalog was fetched anyway")
	}
	if !strings.Contains(call.Result, "data-dictionary") {
		t.Errorf("the correction does not name the real ids: %s", call.Result)
	}

	// Nothing left the process for it, which is the claim that matters.
	for _, r := range in.Context.Requests() {
		if r.Method == "resources/read" {
			t.Errorf("an invented id produced a request for %q", r.URI)
		}
	}
}

// The trace answers "what did Veritix send, and to whom". Before M5b there was
// one answer; there are now two, and the second one has to be in there.
func TestTheTraceRecordsWhatWasAskedOfTheContextServer(t *testing.T) {
	in := metersFixture(t, true)

	provider := llmtest.New(llmtest.Turn{Calls: []llmtest.Call{{
		ID: "1", Name: "read_context", Input: map[string]any{"id": "warehouse-catalog"},
	}}})

	res, err := Run(t.Context(), in, metersOptions(provider), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	ct := res.Trace.Context
	if ct == nil {
		t.Fatal("the trace records no context at all")
	}
	if len(ct.Servers) != 1 || ct.Servers[0].Documents != 3 {
		t.Errorf("servers are %+v", ct.Servers)
	}
	if ct.Read != 1 || ct.Bytes == 0 {
		t.Errorf("the trace says %d documents and %d bytes were admitted", ct.Read, ct.Bytes)
	}

	var reads int
	for _, r := range ct.Requests {
		if r.Method == "resources/read" {
			reads++
			if r.URI == "" {
				t.Error("a read was recorded without the URI it asked for")
			}
		}
	}
	if reads != 1 {
		t.Errorf("the trace records %d reads, want 1", reads)
	}

	// The catalog in the trace is the catalog the model saw, and carries no
	// URI for the same reason.
	body, err := json.Marshal(ct.Documents)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "veritix-test://") {
		t.Errorf("the catalog in the trace carries a URI: %s", body)
	}
}

// The unaided half of a scorecard is only a control if a run without context
// is the run that happened before M5b. So: same tools, same prompt, nothing
// added.
func TestWithoutAContextServerNothingAboutTheRunChanges(t *testing.T) {
	in := metersFixture(t, false)

	var system string
	var toolNames []string
	provider := llmtest.New()
	provider.Reply = func(req *llm.Request) llmtest.Turn {
		system = req.System
		for _, tool := range req.Tools {
			toolNames = append(toolNames, tool.Name)
		}
		return llmtest.Turn{Text: "Nothing further."}
	}

	res, err := Run(t.Context(), in, metersOptions(provider), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if system != systemPrompt {
		t.Error("a run with no context server was sent a different system prompt")
	}
	for _, name := range toolNames {
		if strings.HasSuffix(name, "_context") {
			t.Errorf("a run with no context server was offered %s", name)
		}
	}
	if res.Trace.Context != nil {
		t.Error("a run with no context server recorded a context trace")
	}
}

// Guard.Document is the one path that admits text untouched, and it does so
// whatever the value policy says. A guard that withheld documents with
// AllowValues off would be the feature configured and switched off at once,
// which is the state nobody can reason about.
func TestDocumentsAreAdmittedUnderTheDefaultPolicy(t *testing.T) {
	g := redact.New(redact.Policy{})
	const text = "status: exactly one of active, inactive, removed"
	if got := g.Document(text).String(); got != text {
		t.Errorf("the default policy rewrote a document to %q", got)
	}
	if s := g.Stats(); s.Documents != 1 || s.DocumentBytes != len(text) {
		t.Errorf("stats are %+v", s)
	}
	if s := g.Stats(); s.Shaped != 0 {
		t.Errorf("admitting a document counted %d values as shaped", s.Shaped)
	}
}
