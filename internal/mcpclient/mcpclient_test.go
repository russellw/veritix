package mcpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMain doubles as an MCP server when the environment says so, which is how
// the stdio path gets tested for real. Connecting a client to a subprocess is
// the only way to exercise CommandTransport, the framing, and a server that
// writes its diagnostics somewhere other than stdout — and internal/mcp's own
// gotcha says why a pipe full of hand-written JSON-RPC is not a substitute.
func TestMain(m *testing.M) {
	if dir := os.Getenv("VERITIX_TEST_SERVE_DIR"); dir != "" {
		serveDirectory(dir)
		return
	}
	os.Exit(m.Run())
}

func serveDirectory(dir string) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "docs", Version: "v1"}, nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		os.Exit(1)
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		srv.AddResource(
			&sdk.Resource{URI: "file://" + path, Name: e.Name(), MIMEType: "text/markdown"},
			func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
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

// fake is an in-process context server, and a record of what it was asked for.
type fake struct {
	reads []string
	// sent is every frame that crossed toward the server, verbatim.
	sent bytes.Buffer
}

type doc struct {
	name string
	text string
	blob bool
}

// connect stands a server up in this process and returns a library connected
// to it. The client half is wrapped in a LoggingTransport so a test can read
// the bytes that actually left rather than the arguments a method was given.
func connect(t *testing.T, docs ...doc) (*Library, *fake) {
	t.Helper()

	f := &fake{}
	srv := sdk.NewServer(&sdk.Implementation{Name: "docs", Version: "v1"}, nil)
	for _, d := range docs {
		text := d.text
		res := &sdk.Resource{
			URI:         "veritix-test://doc/" + slug(d.name),
			Name:        d.name,
			Description: "a document about " + d.name,
			MIMEType:    "text/markdown",
		}
		blob := d.blob
		srv.AddResource(res, func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			f.reads = append(f.reads, req.Params.URI)
			c := &sdk.ResourceContents{URI: req.Params.URI, Text: text}
			if blob {
				c.Text, c.Blob = "", []byte{0x25, 0x50, 0x44, 0x46}
			}
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{c}}, nil
		})
	}

	serverT, clientT := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), serverT, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = ss.Wait() })

	lib, err := Connect(t.Context(), Options{Servers: []Server{{
		Name:      "docs",
		Transport: &sdk.LoggingTransport{Transport: clientT, Writer: &f.sent},
	}}})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })
	return lib, f
}

func TestTheCatalogNamesEveryDocument(t *testing.T) {
	lib, _ := connect(t,
		doc{name: "data-dictionary", text: "# what the columns mean"},
		doc{name: "warehouse catalog", text: "# tariff lifecycle"})

	got := lib.Catalog()
	if len(got) != 2 {
		t.Fatalf("catalog has %d documents, want 2", len(got))
	}
	if got[0].ID != "data-dictionary" || got[1].ID != "warehouse-catalog" {
		t.Errorf("ids are %q and %q", got[0].ID, got[1].ID)
	}
	for _, d := range got {
		if d.Server != "docs" {
			t.Errorf("%s came from %q, want docs", d.ID, d.Server)
		}
		if d.Description == "" {
			t.Errorf("%s has no description, which is what decides whether it is worth a step", d.ID)
		}
	}
}

// The catalog is what goes to the model, and a URI it can see is a URI it will
// try to invent — which is the same failure a bare shape produced in the tool
// results. So the type carries none, and this asserts it rather than trusting
// that nobody adds one.
func TestTheCatalogCarriesNoURI(t *testing.T) {
	lib, _ := connect(t, doc{name: "dictionary", text: "# columns"})

	body, err := json.Marshal(lib.Catalog())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(body, []byte("veritix-test://")) {
		t.Errorf("the catalog carries a URI: %s", body)
	}
}

func TestReadingADocumentReturnsItVerbatim(t *testing.T) {
	const text = "# Data dictionary\n\nstatus: one of active, inactive, removed.\n"
	lib, srv := connect(t, doc{name: "dictionary", text: text})

	got, err := lib.Read(t.Context(), "dictionary")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Text != text {
		t.Errorf("read %q, want %q", got.Text, text)
	}
	if got.Truncated {
		t.Error("a short document was reported as truncated")
	}
	if len(srv.reads) != 1 || srv.reads[0] != "veritix-test://doc/dictionary" {
		t.Errorf("the server was asked for %v", srv.reads)
	}
}

// This is the milestone's egress claim in one test. The model names a document
// by a catalog id; anything else is refused here, and the server is never
// asked. A read that reached the server carrying model-written text would be
// Veritix handing somebody else's process a string it did not choose.
func TestAnIDTheModelInventedNeverReachesTheServer(t *testing.T) {
	lib, srv := connect(t, doc{name: "dictionary", text: "# columns"})

	for _, id := range []string{
		"veritix-test://doc/dictionary",
		"../../../etc/passwd",
		"file:///etc/shadow",
		"dictionary; SELECT 1",
		"",
	} {
		if _, err := lib.Read(t.Context(), id); !errors.Is(err, ErrUnknown) {
			t.Errorf("reading %q returned %v, want ErrUnknown", id, err)
		}
	}
	if len(srv.reads) != 0 {
		t.Errorf("the server was asked for %v; nothing should have left", srv.reads)
	}

	// And nothing invented reached the wire either, which is the claim the
	// previous check makes indirectly.
	sent := srv.sent.String()
	for _, needle := range []string{"etc/passwd", "etc/shadow", "SELECT 1"} {
		if strings.Contains(sent, needle) {
			t.Errorf("%q crossed the connection:\n%s", needle, sent)
		}
	}
}

// Every read is of a URI that came out of a listing. That is what makes the
// trace's outbound half checkable by reading it.
func TestEveryRequestIsRecordedAndEveryReadWasAdvertised(t *testing.T) {
	lib, _ := connect(t, doc{name: "dictionary", text: "# columns"})
	if _, err := lib.Read(t.Context(), "dictionary"); err != nil {
		t.Fatalf("read: %v", err)
	}

	reqs := lib.Requests()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want a list and a read: %+v", len(reqs), reqs)
	}
	if reqs[0].Method != "resources/list" || reqs[0].URI != "" {
		t.Errorf("first request is %+v", reqs[0])
	}
	if reqs[1].Method != "resources/read" || reqs[1].URI != "veritix-test://doc/dictionary" {
		t.Errorf("second request is %+v", reqs[1])
	}
	if reqs[1].Bytes == 0 {
		t.Error("the read recorded no bytes")
	}

	advertised := make(map[string]bool)
	for _, d := range lib.Catalog() {
		advertised[lib.byID[d.ID].uri] = true
	}
	for _, r := range reqs {
		if r.URI != "" && !advertised[r.URI] {
			t.Errorf("%s asked for %q, which no listing advertised", r.Method, r.URI)
		}
	}

	if read, bytes := lib.Stats(); read != 1 || bytes == 0 {
		t.Errorf("stats are %d documents and %d bytes", read, bytes)
	}
}

func TestALongDocumentIsCutAndSaysSo(t *testing.T) {
	long := strings.Repeat("a permitted status. ", 500)
	lib, _ := connect(t, doc{name: "dictionary", text: long})
	lib.opts.MaxDocumentBytes = 100

	got, err := lib.Read(t.Context(), "dictionary")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Text) != 100 {
		t.Errorf("read %d bytes, want 100", len(got.Text))
	}
	if !got.Truncated {
		t.Error("a cut document did not say so, so the model would read half a table as the whole of it")
	}
}

// A PDF comes back as a blob. Saying so beats returning an empty document,
// which reads as a document with nothing in it.
func TestANonTextDocumentSaysSoRatherThanReadingEmpty(t *testing.T) {
	lib, _ := connect(t, doc{name: "handbook", blob: true})

	_, err := lib.Read(t.Context(), "handbook")
	if err == nil {
		t.Fatal("a blob was read as if it were text")
	}
	if !strings.Contains(err.Error(), "not text") {
		t.Errorf("error is %q, which does not say why there is nothing to read", err)
	}
}

// An audit that dies because a data dictionary was down is worse than one that
// runs unaided, and the eval's unaided half is what measures the difference.
func TestAServerThatWillNotStartDoesNotFailTheRun(t *testing.T) {
	lib, err := Connect(t.Context(), Options{Servers: []Server{{
		Name:    "dictionary",
		Command: filepath.Join(t.TempDir(), "no-such-program"),
	}}})
	if err != nil {
		t.Fatalf("Connect returned an error rather than continuing without the server: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	conns := lib.Connections()
	if len(conns) != 1 || conns[0].Error == "" {
		t.Fatalf("connections are %+v, want one carrying the reason it failed", conns)
	}
	if len(lib.Catalog()) != 0 {
		t.Error("a server that never answered contributed documents")
	}
}

func TestNoServersMeansNoLibrary(t *testing.T) {
	lib, err := Connect(t.Context(), Options{})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if lib != nil {
		t.Error("configuring nothing produced a library, so the model would be offered context tools")
	}
	// The nil library is used as a value throughout the agent, so it has to
	// behave like an empty one rather than panicking.
	if lib.Catalog() != nil || lib.Requests() != nil || lib.Close() != nil {
		t.Error("a nil library did not behave as an empty one")
	}
	if _, err := lib.Read(t.Context(), "anything"); !errors.Is(err, ErrUnknown) {
		t.Errorf("reading from a nil library returned %v", err)
	}
}

// The real transport, over a real subprocess, because that is what a customer's
// server is.
func TestAStdioServerIsSpokenToAsASubprocess(t *testing.T) {
	dir := t.TempDir()
	const text = "# Tariff catalog\n\nSTD-A closed to new meters on 2024-06-30.\n"
	if err := os.WriteFile(filepath.Join(dir, "catalog.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}

	lib, err := Connect(t.Context(), Options{Servers: []Server{{
		Name:    "docs",
		Command: os.Args[0],
		Env:     []string{"VERITIX_TEST_SERVE_DIR=" + dir},
	}}})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = lib.Close() })

	cat := lib.Catalog()
	if len(cat) != 1 {
		t.Fatalf("catalog is %+v", cat)
	}
	got, err := lib.Read(t.Context(), cat[0].ID)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Text != text {
		t.Errorf("read %q over stdio, want %q", got.Text, text)
	}
}

func TestTwoDocumentsOfTheSameNameGetDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	if got := uniqueID("Data Dictionary", seen); got != "data-dictionary" {
		t.Errorf("first id is %q", got)
	}
	if got := uniqueID("data dictionary", seen); got != "data-dictionary-2" {
		t.Errorf("second id is %q", got)
	}
	if got := uniqueID("!!!", seen); got != "document" {
		t.Errorf("an unslugable name became %q", got)
	}
}
