package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/store"
)

// fixtureDataset is the deliberately broken dataset the rest of the suite
// audits. Driving the real pipeline rather than a stub is the point: the API's
// job is to expose audit.Run faithfully, and a fake would test the fake.
const fixtureDataset = "../../testdata/dirty-retail"

type testServer struct {
	*httptest.Server
	token string
	t     *testing.T
}

func newTestServer(t *testing.T, token string) *testServer {
	t.Helper()
	return newTestServerWith(t, token, nil)
}

// newTestServerWith builds a server serving webFS as the web interface. Most
// tests pass nil: they exercise the API, and a server with no interface built
// into it is a real configuration rather than a contrivance.
func newTestServerWith(t *testing.T, token string, webFS fs.FS) *testServer {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "veritix.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Default()
	cfg.Server.DataDir = dir
	cfg.Server.AuthToken = token

	srv, err := New(context.Background(), Options{Store: st, Config: cfg, Version: "test", Web: webFS})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	return &testServer{Server: hs, token: token, t: t}
}

// response is a request's outcome, already read and closed. Returning this
// rather than an *http.Response keeps every test from having to remember to
// close a body, and keeps the reads in one place.
type response struct {
	Status int
	Header http.Header
	Body   []byte
}

// do issues a request with the bearer token attached, encoding body as JSON
// when one is given.
func (ts *testServer) do(method, path string, body any) response {
	ts.t.Helper()

	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			ts.t.Fatalf("encode request: %v", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, reader)
	if err != nil {
		ts.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ts.token != "" {
		req.Header.Set("Authorization", "Bearer "+ts.token)
	}
	return ts.send(req)
}

// send performs a prepared request and reads the whole response.
func (ts *testServer) send(req *http.Request) response {
	ts.t.Helper()

	resp, err := ts.Client().Do(req)
	if err != nil {
		ts.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close() //nolint:errcheck // fully read below

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		ts.t.Fatalf("read response: %v", err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: buf}
}

// decode checks the status and unmarshals the body.
func (ts *testServer) decode(resp response, wantStatus int, into any) {
	ts.t.Helper()

	if resp.Status != wantStatus {
		ts.t.Fatalf("status = %d, want %d: %s", resp.Status, wantStatus, resp.Body)
	}
	if into != nil {
		if err := json.Unmarshal(resp.Body, into); err != nil {
			ts.t.Fatalf("decode response: %v: %s", err, resp.Body)
		}
	}
}

// get issues an unauthenticated GET, for the endpoints that take no token.
func (ts *testServer) get(path string) response {
	ts.t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+path, nil)
	if err != nil {
		ts.t.Fatalf("build request: %v", err)
	}
	return ts.send(req)
}

// registerFixture points the server at the dirty-retail fixture.
func (ts *testServer) registerFixture() string {
	ts.t.Helper()

	abs, err := filepath.Abs(fixtureDataset)
	if err != nil {
		ts.t.Fatalf("resolve fixture: %v", err)
	}

	var ds datasetJSON
	ts.decode(ts.do(http.MethodPost, "/api/v1/datasets",
		map[string]any{"path": abs}), http.StatusCreated, &ds)
	return ds.ID
}

// startRun begins an audit and waits for it to reach a terminal state, using
// the event stream rather than polling, so the stream is exercised on every
// test that needs a finished run.
func (ts *testServer) startRun(body map[string]any) *runJSON {
	ts.t.Helper()

	var run runJSON
	ts.decode(ts.do(http.MethodPost, "/api/v1/runs", body), http.StatusAccepted, &run)

	done := ts.awaitDone(run.ID)
	if done.Status != string(store.StatusSucceeded) {
		ts.t.Fatalf("run %s: %s (%s)", run.ID, done.Status, done.Message)
	}
	return done
}

// awaitDone streams a run's events and returns the run the terminal event
// carries.
func (ts *testServer) awaitDone(runID string) *runJSON {
	ts.t.Helper()
	run, _ := ts.stream(runID)
	return run
}

// streamMessages returns the progress messages a run published, for tests about
// what a watching browser is told.
func (ts *testServer) streamMessages(runID string) []string {
	ts.t.Helper()
	_, messages := ts.stream(runID)
	return messages
}

// stream reads a run's event stream to its end.
func (ts *testServer) stream(runID string) (*runJSON, []string) {
	ts.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/api/v1/runs/"+runID+"/events", nil)
	if err != nil {
		ts.t.Fatalf("build request: %v", err)
	}
	if ts.token != "" {
		req.Header.Set("Authorization", "Bearer "+ts.token)
	}

	resp, err := ts.Client().Do(req)
	if err != nil {
		ts.t.Fatalf("open event stream: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck // the test reports what matters

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		ts.t.Fatalf("content type = %q, want text/event-stream", got)
	}

	var messages []string
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			ts.t.Fatalf("decode event: %v: %s", err, data)
		}
		if ev.Type == eventDone {
			if ev.Run == nil {
				ts.t.Fatal("the terminal event carried no run")
			}
			return ev.Run, messages
		}
		messages = append(messages, ev.Message)
	}
	ts.t.Fatalf("the event stream ended without a terminal event: %v", scanner.Err())
	return nil, nil
}

// A run has to survive the whole way through: accepted, streamed, stored, and
// then readable as a report after the engine that produced it has closed.
func TestRunLifecycleOverHTTP(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()

	run := ts.startRun(map[string]any{"dataset_id": datasetID})
	if run.Findings.Errors == 0 {
		t.Errorf("the fixture has planted errors but the run reported none: %+v", run.Findings)
	}

	var doc map[string]any
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/report", nil), http.StatusOK, &doc)
	if doc["schema"] != "veritix.audit/v1" {
		t.Errorf("report schema = %v", doc["schema"])
	}

	// The report has to survive the round trip through the store: it is served
	// from there, not from a result still held in memory.
	summary, _ := doc["finding_summary"].(map[string]any)
	if got := int(summary["errors"].(float64)); got != run.Findings.Errors {
		t.Errorf("report has %d errors, the run record says %d", got, run.Findings.Errors)
	}

	resp := ts.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/report.html", nil)
	page := resp.Body
	if resp.Status != http.StatusOK {
		t.Fatalf("report.html status = %d: %s", resp.Status, page)
	}
	if !strings.Contains(string(page), "<html") {
		t.Error("report.html did not return a page")
	}
	// Self-contained: the whole proposition is that a report can be emailed.
	if strings.Contains(string(page), "<script src=") || strings.Contains(string(page), "href=\"http") {
		t.Error("the HTML report references something external")
	}
}

// A run's history has to be readable after the fact, because the store is the
// audit trail: who audited what, when, and what was found.
func TestRunHistory(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID})

	var listed struct {
		Runs []*runJSON `json:"runs"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs?dataset_id="+datasetID, nil),
		http.StatusOK, &listed)

	if len(listed.Runs) != 1 {
		t.Fatalf("listed %d runs, want 1", len(listed.Runs))
	}
	if listed.Runs[0].ID != run.ID {
		t.Errorf("listed run %s, want %s", listed.Runs[0].ID, run.ID)
	}
	if listed.Runs[0].Findings.Total == 0 {
		t.Error("the listed run lost its finding counts")
	}
}

// The default report must not carry verbatim cell values, and neither must the
// API that serves it. This is the same boundary TestDefaultReportContainsNoRawValues
// guards for the file formats: a report is a thing that gets emailed, and the
// API is a thing that gets scraped into a dashboard.
func TestReportOmitsRawValuesByDefault(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID})

	body := ts.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/report", nil).Body

	// Values that exist only inside the fixture's cells. If one of these
	// reaches the default report, something started serializing raw data.
	for _, secret := range []string{"eve@example.com", "Eve Black", "CUS-000005"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("the default report contains the cell value %q", secret)
		}
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	redaction, _ := doc["redaction"].(map[string]any)
	if redaction["values_included"] != false {
		t.Error("the report does not declare that values were withheld")
	}
	if redaction["note"] == nil || redaction["note"] == "" {
		t.Error("a redacted report must say so, or a reader cannot tell it apart from clean data")
	}
}

// The rows endpoint is the one place raw data is served, and it has to be
// asked for one finding at a time.
func TestFindingRowsAreServedOnlyOnRequest(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID})

	var doc struct {
		Findings []struct {
			ID    string `json:"id"`
			Rule  string `json:"rule"`
			Query string `json:"evidence_query"`
		} `json:"findings"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/report", nil), http.StatusOK, &doc)
	if len(doc.Findings) == 0 {
		t.Fatal("the fixture produced no findings")
	}

	// A listed finding must not carry its own rows: that is what makes the
	// rows endpoint a deliberate act rather than a default.
	var withRows string
	for _, f := range doc.Findings {
		resp := ts.do(http.MethodGet,
			fmt.Sprintf("/api/v1/runs/%s/findings/%s/rows?limit=5", run.ID, f.ID), nil)
		var rows struct {
			FindingID string      `json:"finding_id"`
			Columns   []string    `json:"columns"`
			Rows      [][]*string `json:"rows"`
		}
		if resp.Status == http.StatusConflict {
			// A structural observation with no SQL behind it. Legitimate.
			continue
		}
		ts.decode(resp, http.StatusOK, &rows)

		if rows.FindingID != f.ID {
			t.Errorf("asked for %s, got rows for %s", f.ID, rows.FindingID)
		}
		if len(rows.Rows) > 0 {
			withRows = f.ID
			if len(rows.Columns) == 0 {
				t.Errorf("finding %s returned rows with no column names", f.ID)
			}
			if len(rows.Rows) > 5 {
				t.Errorf("limit=5 returned %d rows", len(rows.Rows))
			}
		}
	}
	if withRows == "" {
		t.Error("no finding could show its offending rows; the endpoint is the point of the UI")
	}

	resp := ts.do(http.MethodGet,
		"/api/v1/runs/"+run.ID+"/findings/nosuchfinding/rows", nil)
	ts.decode(resp, http.StatusNotFound, nil)
}

// A finding id has to identify the same problem across runs, or the UI's link
// to a finding's rows points somewhere else after the next audit.
func TestFindingIDsAreStableAcrossRuns(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()

	ids := func() []string {
		run := ts.startRun(map[string]any{"dataset_id": datasetID})
		var doc struct {
			Findings []struct {
				ID string `json:"id"`
			} `json:"findings"`
		}
		ts.decode(ts.do(http.MethodGet, "/api/v1/runs/"+run.ID+"/report", nil), http.StatusOK, &doc)

		out := make([]string, 0, len(doc.Findings))
		for _, f := range doc.Findings {
			out = append(out, f.ID)
		}
		return out
	}

	first, second := ids(), ids()
	if len(first) == 0 {
		t.Fatal("the fixture produced no findings")
	}
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("finding ids changed between two runs of the same data:\n%v\n%v", first, second)
	}
}

// Uploading is how a business user supplies data, and a multipart filename is
// whatever the client says it is. "../../x" must land inside the dataset
// directory as "x" or not at all.
func TestUploadedFilenamesCannotEscapeTheDataDirectory(t *testing.T) {
	ts := newTestServer(t, "")

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, name := range []string{"../../../escaped.csv", "customers.csv"} {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("build upload: %v", err)
		}
		if _, err := part.Write([]byte("id,name\n1,Ada\n")); err != nil {
			t.Fatalf("build upload: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("build upload: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		ts.URL+"/api/v1/datasets", &body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	var ds datasetJSON
	ts.decode(ts.send(req), http.StatusCreated, &ds)
	if !ds.Uploaded {
		t.Error("an uploaded dataset must be marked as one, or deleting it will not clean up")
	}

	entries, err := os.ReadDir(ds.Path)
	if err != nil {
		t.Fatalf("read the upload directory: %v", err)
	}
	got := make([]string, 0, len(entries))
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)

	want := []string{"customers.csv", "escaped.csv"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("upload wrote %v, want %v", got, want)
	}

	// And nothing landed outside it.
	if _, err := os.Stat(filepath.Join(ds.Path, "..", "..", "..", "escaped.csv")); err == nil {
		t.Error("an upload escaped the dataset directory")
	}
}

// Canceling something that has already finished is a mistake worth reporting,
// not a silent no-op that leaves the caller thinking they stopped it.
func TestCancellingAFinishedRunConflicts(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()
	run := ts.startRun(map[string]any{"dataset_id": datasetID})

	ts.decode(ts.do(http.MethodPost, "/api/v1/runs/"+run.ID+"/cancel", nil),
		http.StatusConflict, nil)
}

// TestHealthOffersTheSource pins the AGPL section 13 offer: the interface
// renders it in every screen's footer, including the token gate, so it has to
// come back from the one endpoint that needs no credential — and it has to
// follow the operator's configuration, because a modified build owes its users
// *its* source rather than upstream's.
func TestHealthOffersTheSource(t *testing.T) {
	ts := newTestServer(t, "")

	var got struct {
		Version   string `json:"version"`
		SourceURL string `json:"source_url"`
	}
	ts.decode(ts.get("/api/v1/health"), http.StatusOK, &got)

	if got.SourceURL != config.Default().Server.SourceURL {
		t.Errorf("health source_url = %q, want the configured default %q",
			got.SourceURL, config.Default().Server.SourceURL)
	}
	if got.Version != "test" {
		t.Errorf("health version = %q, want the version the server was built with", got.Version)
	}
}

func TestAuthTokenIsRequiredWhenConfigured(t *testing.T) {
	ts := newTestServer(t, "s3cret")

	// Health stays open: a container probe should not need a credential.
	if got := ts.get("/api/v1/health").Status; got != http.StatusOK {
		t.Errorf("health without a token = %d, want 200", got)
	}

	for _, header := range []string{"", "Bearer wrong", "s3cret", "Basic s3cret"} {
		req, err := http.NewRequestWithContext(context.Background(),
			http.MethodGet, ts.URL+"/api/v1/datasets", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if header != "" {
			req.Header.Set("Authorization", header)
		}

		if got := ts.send(req).Status; got != http.StatusUnauthorized {
			t.Errorf("Authorization %q = %d, want 401", header, got)
		}
	}

	// And the right token works.
	var listed struct {
		Datasets []datasetJSON `json:"datasets"`
	}
	ts.decode(ts.do(http.MethodGet, "/api/v1/datasets", nil), http.StatusOK, &listed)
}

// The same folder registered twice is one dataset, so that run history does
// not fragment across duplicate entries for the same data.
func TestRegisteringTheSamePathTwiceIsOneDataset(t *testing.T) {
	ts := newTestServer(t, "")
	first := ts.registerFixture()
	second := ts.registerFixture()

	if first != second {
		t.Errorf("got two datasets for one path: %s and %s", first, second)
	}
}

func TestRegisteringAMissingPathIsRejected(t *testing.T) {
	ts := newTestServer(t, "")

	resp := ts.do(http.MethodPost, "/api/v1/datasets",
		map[string]any{"path": filepath.Join(t.TempDir(), "does-not-exist")})
	ts.decode(resp, http.StatusBadRequest, nil)
}

// An unknown field is refused rather than ignored: "include_value" silently
// dropped would produce a report the caller believed contained examples, or
// worse, believed did not.
func TestUnknownRequestFieldsAreRefused(t *testing.T) {
	ts := newTestServer(t, "")
	datasetID := ts.registerFixture()

	resp := ts.do(http.MethodPost, "/api/v1/runs",
		map[string]any{"dataset_id": datasetID, "include_value": true})
	ts.decode(resp, http.StatusBadRequest, nil)
}

func TestReportOfAnUnknownRunIsNotFound(t *testing.T) {
	ts := newTestServer(t, "")

	ts.decode(ts.do(http.MethodGet, "/api/v1/runs/nope", nil), http.StatusNotFound, nil)
	ts.decode(ts.do(http.MethodGet, "/api/v1/runs/nope/report", nil), http.StatusNotFound, nil)
	ts.decode(ts.do(http.MethodGet, "/api/v1/nonsense", nil), http.StatusNotFound, nil)
}

// The contract is served, and it is the contract this build actually parses.
func TestOpenAPISpecIsServed(t *testing.T) {
	ts := newTestServer(t, "")

	resp := ts.get("/api/v1/openapi.yaml")
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "openapi:") {
		t.Error("the served document is not an OpenAPI spec")
	}
}

// Two uploads of the same folder must not land in the same directory.
//
// They used to. The upload directory was named with the first eight characters
// of a UUIDv7, described in the code as random; they are the high bits of the
// millisecond timestamp and do not change for about a minute. MkdirAll then
// returned the existing directory happily, and the second upload failed on the
// first file that was already there — which a user sees as "could not store
// legacy.xls: file exists" after uploading a folder they have uploaded before.
func TestUploadingTheSameFolderTwiceDoesNotCollide(t *testing.T) {
	ts := newTestServer(t, "")

	upload := func() datasetJSON {
		t.Helper()
		body := &bytes.Buffer{}
		mw := multipart.NewWriter(body)
		if err := mw.WriteField("name", "quarterly exports"); err != nil {
			t.Fatalf("write field: %v", err)
		}
		part, err := mw.CreateFormFile("files", "orders.csv")
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := io.WriteString(part, "id,amount\n1,10.00\n"); err != nil {
			t.Fatalf("write part: %v", err)
		}
		if err := mw.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}

		req, err := http.NewRequestWithContext(t.Context(),
			http.MethodPost, ts.URL+"/api/v1/datasets", body)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())

		var ds datasetJSON
		ts.decode(ts.send(req), http.StatusCreated, &ds)
		return ds
	}

	first := upload()
	second := upload()

	if first.Path == second.Path {
		t.Fatalf("both uploads landed in %s", first.Path)
	}
	if first.ID == second.ID {
		t.Errorf("both uploads produced dataset %s", first.ID)
	}
	for _, path := range []string{first.Path, second.Path} {
		if _, err := os.Stat(filepath.Join(path, "orders.csv")); err != nil {
			t.Errorf("%s is missing its file: %v", path, err)
		}
	}
}
