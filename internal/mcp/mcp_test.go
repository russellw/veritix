package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/report"
	"github.com/russellw/veritix/internal/store"
)

// fixtureDataset is the deliberately broken dataset the rest of the suite
// audits. Like the API's tests, these drive the real pipeline rather than a
// stub: this package's job is to expose audit.Run faithfully to an assistant,
// and a fake would only test the fake.
const fixtureDataset = "../../testdata/dirty-retail"

// rawValuesInFixture are verbatim contents of the fixture files. The same list
// the report, CLI, and agent suites use. None of them may appear in anything
// this server sends, because the thing on the other end of an MCP connection
// is a model in somebody else's context.
var rawValuesInFixture = []string{
	"CUS-000001", "CUS-000005", "CUS-999999",
	"alice@example.com", "carol@example.com",
	"Alice Smith", "Frank Green",
	"Zürich", "München", "Montréal",
	"Doohickey", "Widget",
	"Quarterly Sales Report",
}

// session is a connected client, plus everything the server sent it.
type session struct {
	*sdk.ClientSession
	t    *testing.T
	sent *bytes.Buffer
	// dir is the server's data directory, for a test that needs to look at
	// what Veritix wrote there.
	dir string
}

func connect(t *testing.T, opts Options) *session {
	t.Helper()
	return connectWithStore(t, opts, nil)
}

func connectWithStore(t *testing.T, opts Options, st *store.Store) *session {
	t.Helper()

	dir := t.TempDir()
	if st == nil {
		var err error
		if st, err = store.Open(filepath.Join(dir, "veritix.db")); err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
	}

	cfg := config.Default()
	cfg.Server.DataDir = dir

	opts.Store = st
	opts.Config = cfg
	if opts.Version == "" {
		opts.Version = "test"
	}

	srv, err := New(opts)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	serverT, clientT := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), serverT)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })

	// Every frame in both directions is recorded, so the egress test can read
	// the bytes that actually crossed the connection rather than the values a
	// handler happened to return.
	var sent bytes.Buffer
	client := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "v1"}, nil)
	cs, err := client.Connect(context.Background(),
		&sdk.LoggingTransport{Transport: clientT, Writer: &sent}, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	return &session{ClientSession: cs, t: t, sent: &sent, dir: dir}
}

// call invokes a tool and decodes its structured result, failing the test if
// the server reported an error.
func (s *session) call(name string, args map[string]any, into any) {
	s.t.Helper()
	res := s.callRaw(name, args)
	if res.IsError {
		s.t.Fatalf("%s failed: %s", name, text(res))
	}
	if into == nil {
		return
	}
	body, err := json.Marshal(res.StructuredContent)
	if err != nil {
		s.t.Fatalf("%s returned content that could not be re-encoded: %v", name, err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		s.t.Fatalf("%s returned %s, which does not fit %T: %v", name, body, into, err)
	}
}

func (s *session) callRaw(name string, args map[string]any) *sdk.CallToolResult {
	s.t.Helper()
	res, err := s.CallTool(s.t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *sdk.CallToolResult) string {
	var out []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return strings.Join(out, "\n")
}

// audit runs the fixture through the server and returns what it found.
func (s *session) audit() auditOut {
	s.t.Helper()
	var out auditOut
	s.call("audit_dataset", map[string]any{"path": fixtureDataset}, &out)
	return out
}

// An assistant asks for an audit and gets findings that were measured rather
// than asserted: this is the whole point of the milestone.
func TestAuditingADatasetReturnsVerifiedFindings(t *testing.T) {
	s := connect(t, Options{})
	out := s.audit()

	if out.Run.Status != string(store.StatusSucceeded) {
		t.Fatalf("run %s ended %s: %s", out.Run.ID, out.Run.Status, out.Run.Message)
	}
	if out.Run.Findings.Total == 0 {
		t.Fatal("the fixture is full of planted defects and the audit reported none")
	}
	if len(out.Findings) == 0 {
		t.Fatal("the run counted findings but returned none of them")
	}
	if out.Dataset.Path == "" || !strings.HasSuffix(out.Dataset.Path, "dirty-retail") {
		t.Errorf("audited %q, which is not the fixture", out.Dataset.Path)
	}

	// A finding with SQL behind it has been re-executed; one without is a
	// structural observation made while reading the file, which has no query
	// by nature. Neither may arrive unmeasured.
	var measured int
	for _, f := range out.Findings {
		if f.Query == "" {
			continue
		}
		measured++
		if !f.Verified {
			t.Errorf("finding %s (%s) was reported without its evidence reproducing", f.ID, f.Rule)
		}
	}
	if measured == 0 {
		t.Error("not one finding carries the SQL that demonstrates it")
	}
}

// The guarantee the product rests on, at the boundary this milestone adds.
// Everything the server said is scanned, not only the fields a handler meant
// to fill: a value that escapes through an error message or a table name is
// out just the same.
func TestNothingSentOverMCPContainsARawValue(t *testing.T) {
	s := connect(t, Options{})

	out := s.audit()
	runID := out.Run.ID

	// Every tool, so that no reachable response is left unscanned.
	s.call("list_datasets", nil, nil)
	s.call("list_runs", nil, nil)
	s.call("get_run", map[string]any{"run_id": runID}, nil)
	s.call("list_findings", map[string]any{"run_id": runID}, nil)
	s.call("get_report", map[string]any{"run_id": runID}, nil)
	s.call("register_dataset", map[string]any{"path": fixtureDataset}, nil)

	sent := s.sent.String()
	if !strings.Contains(sent, "audit_dataset") {
		t.Fatal("the transcript is empty; the test is not reading what was sent")
	}
	for _, raw := range rawValuesInFixture {
		if strings.Contains(sent, raw) {
			t.Errorf("the MCP transcript leaks the raw value %q", raw)
		}
	}
}

// The caller chooses what to audit; the operator chooses what Veritix may
// disclose. A tool parameter that lifted the egress policy would move that
// decision to the model, so there must not be one.
func TestNoToolLetsTheCallerLiftTheEgressPolicy(t *testing.T) {
	s := connect(t, Options{})

	forbidden := []string{"include_values", "allow_sample_values", "agent", "rows"}
	var tools int
	for tool, err := range s.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		tools++

		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s has an input schema that will not marshal: %v", tool.Name, err)
		}
		var parsed struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schema, &parsed); err != nil {
			t.Fatalf("%s: %v", tool.Name, err)
		}
		for name := range parsed.Properties {
			for _, bad := range forbidden {
				if name == bad {
					t.Errorf("%s takes a %q parameter: that decision is the operator's, not the caller's",
						tool.Name, bad)
				}
			}
		}
	}
	if tools == 0 {
		t.Fatal("the server offered no tools")
	}
}

// The rows endpoint is internal/api's one deliberate exception and stays
// there. Nothing here may offer a second way to the same data.
func TestThereIsNoToolThatReturnsRows(t *testing.T) {
	s := connect(t, Options{})
	for tool, err := range s.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		if strings.Contains(tool.Name, "row") || strings.Contains(tool.Name, "sample") {
			t.Errorf("tool %q looks like a way to fetch raw rows", tool.Name)
		}
	}
}

// An audit started by an assistant is the same run, in the same history, as
// one started from the browser — not a parallel record that has to be
// reconciled later.
func TestAnAuditIsRecordedInTheOrdinaryHistory(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	s := connectWithStore(t, Options{}, st)
	out := s.audit()

	run, err := st.Run(t.Context(), out.Run.ID)
	if err != nil {
		t.Fatalf("the run is not in the store: %v", err)
	}
	if run.Status != store.StatusSucceeded {
		t.Errorf("the store says %s, the tool said %s", run.Status, out.Run.Status)
	}
	if run.Total() != out.Run.Findings.Total {
		t.Errorf("the store counts %d findings, the tool reported %d", run.Total(), out.Run.Findings.Total)
	}

	// The dataset's database is left behind for the rows endpoint, exactly as
	// an HTTP-started run leaves one.
	if run.DatabasePath == "" {
		t.Error("the run kept no database, so the web interface could not show its rows")
	}
	if _, err := st.Findings(t.Context(), run.ID); err != nil {
		t.Errorf("the run's findings are not queryable from the store: %v", err)
	}
}

// What an assistant is told and what the JSON report says are one document,
// read back from the store rather than rebuilt.
func TestTheReportIsTheStoredDocument(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	s := connectWithStore(t, Options{}, st)
	out := s.audit()

	var served report.Document
	s.call("get_report", map[string]any{"run_id": out.Run.ID}, &served)

	raw, err := st.Document(t.Context(), out.Run.ID)
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	var stored report.Document
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("decode the stored document: %v", err)
	}

	if served.FindingSummary != stored.FindingSummary {
		t.Errorf("get_report summarizes %+v, the stored report %+v", served.FindingSummary, stored.FindingSummary)
	}
	if len(served.Findings) != len(stored.Findings) {
		t.Fatalf("get_report returned %d findings, the stored report has %d",
			len(served.Findings), len(stored.Findings))
	}
	for i := range served.Findings {
		if served.Findings[i].ID != stored.Findings[i].ID {
			t.Errorf("finding %d differs: %s vs %s", i, served.Findings[i].ID, stored.Findings[i].ID)
		}
	}
	if served.Redacted.ValuesIncluded {
		t.Error("the default report declares that it includes values")
	}
}

// list_findings pages and filters over the same document.
func TestFindingsCanBeFilteredAndPaged(t *testing.T) {
	s := connect(t, Options{})
	out := s.audit()

	var all listFindingsOut
	s.call("list_findings", map[string]any{"run_id": out.Run.ID}, &all)
	if len(all.Findings) == 0 {
		t.Fatal("list_findings returned nothing for a run with findings")
	}

	var errorsOnly listFindingsOut
	s.call("list_findings", map[string]any{"run_id": out.Run.ID, "severity": "error"}, &errorsOnly)
	for _, f := range errorsOnly.Findings {
		if f.Severity != "error" {
			t.Errorf("asked for errors and got a %s finding: %s", f.Severity, f.Rule)
		}
	}
	if len(errorsOnly.Findings) != all.Summary.Errors {
		t.Errorf("the run counts %d errors, the filter returned %d",
			all.Summary.Errors, len(errorsOnly.Findings))
	}

	var offset listFindingsOut
	s.call("list_findings", map[string]any{"run_id": out.Run.ID, "offset": 1}, &offset)
	if len(offset.Findings) != len(all.Findings)-1 {
		t.Errorf("offset 1 returned %d of %d findings", len(offset.Findings), len(all.Findings))
	}
	if len(all.Findings) > 1 && offset.Findings[0].ID != all.Findings[1].ID {
		t.Error("offset 1 did not start at the second finding")
	}
}

// A caller's mistake comes back as something it can correct, not as a broken
// session. This is the same rule the agent's tool registry follows.
func TestAMistakeIsAToolErrorRatherThanAProtocolFailure(t *testing.T) {
	s := connect(t, Options{})

	for _, tc := range []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"unknown run", "get_run", map[string]any{"run_id": "nope"}, "no run with id"},
		{"unknown dataset", "audit_dataset", map[string]any{"dataset_id": "nope"}, "no dataset with id"},
		{"neither path nor id", "audit_dataset", nil, "not both and not neither"},
		{"both path and id", "audit_dataset",
			map[string]any{"path": fixtureDataset, "dataset_id": "x"}, "not both and not neither"},
		{"missing path", "register_dataset", map[string]any{"path": "/no/such/place"}, "cannot read"},
		{"bad severity", "list_findings",
			map[string]any{"run_id": "nope", "severity": "catastrophic"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := s.callRaw(tc.tool, tc.args)
			if !res.IsError {
				t.Fatalf("%s accepted %v", tc.tool, tc.args)
			}
			if tc.want != "" && !strings.Contains(text(res), tc.want) {
				t.Errorf("the error says %q, which does not tell the caller %q", text(res), tc.want)
			}
		})
	}
}

// The agentic pass is the operator's decision, and a broken one is their
// problem to see at startup rather than an assistant's to discover mid-call.
func TestAMisconfiguredAgentFailsAtStartup(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close() //nolint:errcheck // test cleanup

	cfg := config.Default()
	cfg.Server.DataDir = dir
	// The default provider is none, which is a complete auditor but not an
	// agentic one.
	if _, err := New(Options{Store: st, Config: cfg, Version: "test", Agent: true}); err == nil {
		t.Fatal("a server with --agent and no provider started anyway")
	}
}

// The instructions are what a client shows its model before any tool is
// called, so the two facts a caller most needs are checked to be in them.
func TestTheInstructionsStateTheEgressPolicy(t *testing.T) {
	s := connect(t, Options{})
	res := s.InitializeResult()
	if !strings.Contains(res.Instructions, "audit_dataset") {
		t.Error("the instructions do not name the tool to start with")
	}
	if !strings.Contains(res.Instructions, "no verbatim cell values") {
		t.Error("the instructions do not state the egress policy")
	}
}

// A rule accepted in the browser is in force for an audit an assistant starts,
// because both doors open onto the same building. An entry point that quietly
// skipped the dataset's own rules would report a different answer for the same
// data depending on how it was asked for, which is what internal/runs exists
// to prevent.
func TestAnAcceptedRuleAppliesToAnAuditStartedOverMCP(t *testing.T) {
	s := connect(t, Options{})

	var ds struct {
		ID string `json:"id"`
	}
	s.call("register_dataset", map[string]any{"path": fixtureDataset}, &ds)

	// What the accept endpoint writes, written directly: this test is about
	// whether the audit reads it, not about how it got there.
	dir := filepath.Join(s.dir, "datasets", ds.ID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := `version: 1
rules:
  - id: status_domain
    description: status is drawn from a fixed vocabulary
    table: customers_csv
    column: status
    expect: one_of
    values: [Active, Inactive, Suspended, Closed]
    ignore_case: true
    allow_missing: true
`
	if err := os.WriteFile(filepath.Join(dir, "rules.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		Findings []struct {
			Rule  string `json:"rule"`
			Count int64  `json:"affected_count"`
		} `json:"findings"`
	}
	s.call("audit_dataset", map[string]any{"dataset_id": ds.ID}, &out)

	var found bool
	for _, f := range out.Findings {
		if f.Rule == "rule.status_domain" {
			found = true
			if f.Count != 1 {
				t.Errorf("the rule caught %d rows, want 1", f.Count)
			}
		}
	}
	if !found {
		t.Errorf("the dataset's accepted rules did not run: %+v", out.Findings)
	}
}
