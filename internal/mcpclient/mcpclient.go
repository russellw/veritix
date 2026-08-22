// Package mcpclient is the door out of the process, as internal/mcp is the
// door in.
//
// Four of dirty-meters' six agent targets are invisible in the export and
// become visible only when the customer's own documents are read: a status
// vocabulary, a tariff lifecycle date, what a column *means*, and how two
// columns join. Nothing in the data marks the offending rows out, so no amount
// of profiling finds them. What finds them is the data dictionary the customer
// already maintains, and this package is how it gets in.
//
// # The surface, and why it is this small
//
// Veritix connects to the MCP servers the operator configured, enumerates
// their *resources*, and offers the model two tools: list what documents there
// are, and read one by an id Veritix assigned. That is the whole of it. In
// particular:
//
//   - **No text the model wrote ever leaves the process.** The model names a
//     document by a catalog id; the id is looked up and the *catalog's* URI is
//     what gets requested. This is exactly the rule
//     [tools] follows for SQL identifiers — a model-supplied table name is
//     looked up in the profile and the profile's name is what gets quoted —
//     applied to the one other place where something Veritix holds could be
//     handed to somebody else. An id that is not in the catalog is a tool
//     error, not a request.
//   - The model is not shown the URIs either, because a URI it can see is a
//     URI it will try to invent. [Request] records them for the trace, which
//     is where a customer checks what left rather than the model.
//   - The servers' own *tools* are not exposed. A tool call forwards arguments
//     the model wrote, which is the thing the first rule exists to prevent. A
//     ticketing system that can only be searched therefore cannot be reached
//     from here yet; see the note at the end of docs/mcp.md, because turning
//     that on is a decision about egress and not a feature.
//
// # What it does admit
//
// Whatever those servers return goes to the model verbatim: a data dictionary
// rendered as ⟨XXXXXXX⟩ would be worth nothing. That is the operator's
// decision, taken by configuring a server at all, in the same way that
// --include-values is the operator's decision about cell values — and it is
// recorded the same way, in the run's trace, along with every request Veritix
// made to get there. Nothing is configured by default, so a default install
// still talks to nobody.
package mcpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"time"
	"unicode"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/russellw/veritix/internal/buildinfo"
)

// Server is one MCP server the operator has configured as a source of context.
type Server struct {
	// Name identifies the server in the catalog, the log and the trace. It is
	// Veritix's handle for it rather than anything the server says about
	// itself, so a document's provenance survives a server that renames
	// itself.
	Name string `yaml:"name"`
	// Command is the executable to run, spoken to over stdio. That is the
	// transport an MCP server on the customer's own machine uses, which is the
	// only kind that is consistent with the rest of this product: a context
	// server reached over the network is one more place the data could go.
	Command string `yaml:"command"`
	// Args are its arguments.
	Args []string `yaml:"args"`
	// Env is added to the command's environment as NAME=VALUE. The process
	// environment is passed through as well, because a real server needs a
	// PATH and a HOME.
	Env []string `yaml:"env"`

	// Transport overrides Command, and is how a test connects to a server in
	// the same process. It is also where a non-stdio transport would attach if
	// one is ever wanted.
	Transport sdk.Transport `yaml:"-"`
}

// Options configure a connection to the configured servers.
type Options struct {
	Servers []Server
	// MaxDocumentBytes truncates one document. A warehouse catalog that turns
	// out to be two megabytes would otherwise fill the model's context with
	// one read and leave no room for the dataset it is supposed to explain.
	// Zero picks a default.
	MaxDocumentBytes int
	// ConnectTimeout bounds starting one server and listing its resources.
	// Zero picks a default. It is deliberately short: the audit is the point,
	// and a context server that is not answering should cost seconds.
	ConnectTimeout time.Duration
	Log            *slog.Logger
}

const (
	defaultMaxDocumentBytes = 24000
	defaultConnectTimeout   = 30 * time.Second
	// maxCatalog caps how many documents one server contributes. A resource
	// list is somebody else's data structure and can be enormous; the catalog
	// goes in the brief, so it has to be bounded by something other than hope.
	maxCatalog = 200
)

// Document is one catalog entry, as Veritix names it.
//
// The URI is not here. It is what Veritix sends and never what the model sees:
// see the package comment.
type Document struct {
	// ID is what the model passes to read_context. It is derived from the
	// document's name rather than from its URI, so it is short enough to type
	// and stable across runs.
	ID string `json:"id"`
	// Server is which configured server it came from, so that a person reading
	// a finding can tell the dictionary from the ticket tracker.
	Server string `json:"server"`
	// Name and Description are the server's own, and are what tells the model
	// whether a document is worth a step.
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	// Size is the server's own figure, when it gives one.
	Size int64 `json:"size_bytes,omitempty"`
}

// Contents is one document, as read.
type Contents struct {
	Document
	// Text is the document verbatim. It is not redacted: see the package
	// comment for whose decision that is.
	Text string
	// Truncated says the document was longer than MaxDocumentBytes, so that
	// the model can be told rather than silently reading half a table.
	Truncated bool
}

// Request is one thing Veritix asked a context server for.
//
// It is the outbound half of the egress record. The trace already carries
// every byte that reached the *model*; this is every byte that left toward
// somebody else, and it is what makes "no text the model wrote leaves the
// process" checkable by reading rather than by trusting. Every entry is either
// a listing or a read of a URI that came out of a listing.
type Request struct {
	Server string `json:"server"`
	// Method is the MCP method, verbatim: "resources/list" or
	// "resources/read".
	Method string `json:"method"`
	// URI is what was asked for, which for a read is always one the server
	// itself advertised.
	URI string `json:"uri,omitempty"`
	// Bytes is the size of what came back.
	Bytes      int    `json:"bytes,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

// Connection is what one configured server contributed.
type Connection struct {
	Name string `json:"name"`
	// Documents is how many of its resources reached the catalog.
	Documents int `json:"documents"`
	// Omitted is how many were dropped for exceeding maxCatalog.
	Omitted int `json:"omitted,omitempty"`
	// Error is why this server contributed nothing. A context server that is
	// down does not fail the audit: an audit that dies because a data
	// dictionary was unreachable is worse than one that runs unaided, and the
	// eval's unaided half is the measure of what that costs.
	Error string `json:"error,omitempty"`
}

// Library is the catalog the model may read from, and the connections behind
// it.
type Library struct {
	opts Options
	log  *slog.Logger

	sessions []*sdk.ClientSession
	conns    []Connection

	docs  []Document
	byID  map[string]entry
	mu    sync.Mutex
	reqs  []Request
	fetch int
	bytes int
}

// entry is a catalog document plus the two things the model never sees: which
// session to ask, and what to ask it for.
type entry struct {
	doc     Document
	uri     string
	session *sdk.ClientSession
}

// Connect starts every configured server and builds the catalog.
//
// It returns nil, nil when nothing is configured, which is the default: a
// caller can pass the result straight through and a Library that is nil offers
// the model no context tools at all.
//
// A server that cannot be reached is recorded and skipped rather than failing
// the audit. The caller must Close the result.
//
// ctx bounds the subprocesses' lifetime as well as the connection, so it has
// to be the run's context and not a short-lived one: Close is the ordinary way
// they end, and cancellation is the backstop that stops a server outliving the
// audit that started it.
func Connect(ctx context.Context, opts Options) (*Library, error) {
	if len(opts.Servers) == 0 {
		return nil, nil
	}
	if opts.MaxDocumentBytes <= 0 {
		opts.MaxDocumentBytes = defaultMaxDocumentBytes
	}
	if opts.ConnectTimeout <= 0 {
		opts.ConnectTimeout = defaultConnectTimeout
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	lib := &Library{opts: opts, log: log, byID: make(map[string]entry)}
	seen := make(map[string]bool)
	for _, srv := range opts.Servers {
		conn := Connection{Name: srv.Name}
		if err := lib.attach(ctx, srv, seen, &conn); err != nil {
			conn.Error = err.Error()
			log.Warn("a context server could not be reached; the audit continues without it",
				"server", srv.Name, "error", err)
		}
		lib.conns = append(lib.conns, conn)
	}

	log.Info("context servers connected",
		"servers", len(lib.conns), "documents", len(lib.docs))
	return lib, nil
}

// attach connects one server and adds its resources to the catalog.
func (l *Library) attach(ctx context.Context, srv Server, seen map[string]bool, conn *Connection) error {
	if srv.Name == "" {
		return errors.New("a context server needs a name")
	}

	transport := srv.Transport
	if transport == nil {
		if srv.Command == "" {
			return errors.New("no command and no transport")
		}
		// The run's context and not the connect deadline below: the deadline
		// bounds the handshake, and a server killed thirty seconds into an
		// audit would take the documents with it. Close is how these normally
		// end; this is what stops one outliving the audit if it does not.
		cmd := exec.CommandContext(ctx, srv.Command, srv.Args...) //nolint:gosec // the operator's own configured command
		cmd.Env = append(cmd.Environ(), srv.Env...)
		transport = &sdk.CommandTransport{Command: cmd}
	}

	connectCtx, cancel := context.WithTimeout(ctx, l.opts.ConnectTimeout)
	defer cancel()

	client := sdk.NewClient(&sdk.Implementation{
		Name:    "veritix",
		Title:   "Veritix data auditor",
		Version: buildinfo.Version,
	}, nil)
	session, err := client.Connect(connectCtx, transport, nil)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	l.sessions = append(l.sessions, session)

	started := time.Now()
	resources, err := l.list(connectCtx, session)
	l.record(Request{
		Server:     srv.Name,
		Method:     "resources/list",
		DurationMS: time.Since(started).Milliseconds(),
		Error:      errText(err),
	})
	if err != nil {
		return fmt.Errorf("listing resources: %w", err)
	}

	for _, r := range resources {
		if conn.Documents >= maxCatalog {
			conn.Omitted++
			continue
		}
		doc := Document{
			ID:          uniqueID(name(r), seen),
			Server:      srv.Name,
			Name:        name(r),
			Description: r.Description,
			MIMEType:    r.MIMEType,
			Size:        r.Size,
		}
		l.docs = append(l.docs, doc)
		l.byID[doc.ID] = entry{doc: doc, uri: r.URI, session: session}
		conn.Documents++
	}
	return nil
}

// list enumerates a server's resources, following pagination.
//
// The iterator is used rather than one ListResources call because a server
// with more documents than fit in a page is exactly the kind that has the one
// worth reading on page two.
func (l *Library) list(ctx context.Context, session *sdk.ClientSession) ([]*sdk.Resource, error) {
	var out []*sdk.Resource
	for r, err := range session.Resources(ctx, nil) {
		if err != nil {
			return out, err
		}
		out = append(out, r)
		if len(out) >= maxCatalog {
			break
		}
	}
	return out, nil
}

// Catalog is what the model may read, in a stable order.
func (l *Library) Catalog() []Document {
	if l == nil {
		return nil
	}
	return append([]Document(nil), l.docs...)
}

// Connections reports what each configured server contributed, including the
// ones that contributed nothing and why.
func (l *Library) Connections() []Connection {
	if l == nil {
		return nil
	}
	return append([]Connection(nil), l.conns...)
}

// Requests is everything Veritix asked the context servers for, in order.
func (l *Library) Requests() []Request {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]Request(nil), l.reqs...)
}

// Stats reports how many documents were read and how many bytes of them were
// admitted, for the trace's summary line.
func (l *Library) Stats() (documents, bytes int) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.fetch, l.bytes
}

// ErrUnknown is returned when the model names a document that is not in the
// catalog. It is a mistake the model can correct, so it comes back as a tool
// error rather than ending anything.
var ErrUnknown = errors.New("no such document")

// Read fetches one catalog document.
//
// The id is looked up and the catalog's own URI is what gets requested, so
// nothing the model wrote reaches the server. An id that is not in the catalog
// produces ErrUnknown and no request at all.
func (l *Library) Read(ctx context.Context, id string) (*Contents, error) {
	if l == nil {
		return nil, ErrUnknown
	}
	e, ok := l.byID[strings.TrimSpace(id)]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknown, id)
	}

	started := time.Now()
	res, err := e.session.ReadResource(ctx, &sdk.ReadResourceParams{URI: e.uri})
	text, truncated, readErr := l.text(res, err)
	l.record(Request{
		Server:     e.doc.Server,
		Method:     "resources/read",
		URI:        e.uri,
		Bytes:      len(text),
		DurationMS: time.Since(started).Milliseconds(),
		Error:      errText(readErr),
	})
	if readErr != nil {
		return nil, readErr
	}

	l.mu.Lock()
	l.fetch++
	l.bytes += len(text)
	l.mu.Unlock()

	l.log.Info("context document read",
		"server", e.doc.Server, "document", e.doc.ID, "bytes", len(text), "truncated", truncated)
	return &Contents{Document: e.doc, Text: text, Truncated: truncated}, nil
}

// text extracts the readable part of a resource, or says why there is none.
func (l *Library) text(res *sdk.ReadResourceResult, err error) (string, bool, error) {
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	for _, c := range res.Contents {
		if c == nil || c.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(c.Text)
	}
	if b.Len() == 0 {
		// A PDF or an image comes back as a blob. Saying so is more useful
		// than an empty document, which reads as a document with nothing in
		// it — and there is nothing here that could turn one into text.
		return "", false, errors.New("that document is not text, so there is nothing to read")
	}
	out := b.String()
	if len(out) > l.opts.MaxDocumentBytes {
		return out[:l.opts.MaxDocumentBytes], true, nil
	}
	return out, false, nil
}

func (l *Library) record(r Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reqs = append(l.reqs, r)
}

// Close ends every session, which for a stdio server ends the subprocess.
func (l *Library) Close() error {
	if l == nil {
		return nil
	}
	var err error
	for _, s := range l.sessions {
		if closeErr := s.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	l.sessions = nil
	return err
}

func name(r *sdk.Resource) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Title != "" {
		return r.Title
	}
	return r.URI
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// uniqueID derives the id the model will type from the document's name.
//
// The name rather than the URI, because a URI is a path with a scheme on it
// and the model has to write the thing exactly: "data-dictionary" is typeable
// and "file:///srv/docs/dictionary/current.md" is a transcription error
// waiting to happen. Collisions get a numeric suffix rather than a server
// prefix, so that adding a second server does not renumber the first one's
// documents.
func uniqueID(name string, seen map[string]bool) string {
	base := slug(name)
	if base == "" {
		base = "document"
	}
	id := base
	for n := 2; seen[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	seen[id] = true
	return id
}

func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
