// Command context-server serves a directory of documents over MCP, so that
// Veritix's client mode has something to read.
//
// It exists for three jobs and is honest about which is which. It is the
// instrument the aided half of dirty-meters' scorecard is measured with —
// `veritix eval testdata/dirty-meters` needs *something* serving
// `context/`, and the alternative was Veritix reading those files itself,
// which would have measured a feature that does not exist. It is how somebody
// evaluating Veritix points it at a folder of their own documentation without
// first writing an MCP server. And it is the smallest complete example of what
// a customer's own server has to do: list resources, read one by URI.
//
// It is not part of the product and is not in the shipped binary. A real
// deployment connects to the customer's own dictionary, catalog or ticket
// system, which is theirs and already exists.
//
//	go run ./scripts/context-server -dir testdata/dirty-meters/context
//	./bin/veritix audit testdata/dirty-meters --llm anthropic \
//	    --context-server "docs:go run ./scripts/context-server -dir testdata/dirty-meters/context"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// textExtensions are what this will serve. A context server that offered a
// PDF would be offering something Veritix's client correctly refuses, so the
// example does not pretend otherwise.
var textExtensions = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".txt":      "text/plain",
	".csv":      "text/csv",
	".json":     "application/json",
	".yaml":     "text/yaml",
	".yml":      "text/yaml",
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("context-server: %v", err)
	}
}

// run is separate from main so that a failure still runs the deferred signal
// cleanup. log.Fatal skipping a defer is the same thing that made os.Exit in
// cmd/veritix worth fixing.
func run() error {
	log.SetFlags(0)
	// Diagnostics to stderr, always: on a stdio transport stdout is the
	// protocol, and one stray line on it is a client that cannot parse
	// anything afterwards.
	log.SetOutput(os.Stderr)

	dir := flag.String("dir", ".", "the directory of documents to serve")
	name := flag.String("name", "documents", "what this server calls itself")
	flag.Parse()

	root, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	docs, err := discover(root)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return fmt.Errorf("no readable documents under %s", root)
	}

	srv := sdk.NewServer(&sdk.Implementation{
		Name:    *name,
		Title:   "Documents in " + filepath.Base(root),
		Version: "0.1.0",
	}, nil)
	for _, d := range docs {
		srv.AddResource(d.resource(), read(root))
	}
	log.Printf("context-server: serving %d documents from %s", len(docs), root)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx, &sdk.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// document is one file, described the way an MCP client will see it.
type document struct {
	rel      string
	uri      string
	mime     string
	size     int64
	headline string
}

func (d document) resource() *sdk.Resource {
	return &sdk.Resource{
		URI:      d.uri,
		Name:     strings.TrimSuffix(d.rel, filepath.Ext(d.rel)),
		Title:    d.headline,
		MIMEType: d.mime,
		Size:     d.size,
		// The description is what decides whether a model spends a step on
		// this document, so it is the file's own first heading rather than
		// something generic. A server whose every resource is described as "a
		// document" has told the model nothing.
		Description: d.headline,
	}
}

// discover walks the directory for readable documents.
func discover(root string) ([]document, error) {
	var out []document
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") && path != root {
				return fs.SkipDir
			}
			return nil
		}
		mime, ok := textExtensions[strings.ToLower(filepath.Ext(entry.Name()))]
		if !ok {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, document{
			rel:      filepath.ToSlash(rel),
			uri:      "file://" + filepath.ToSlash(path),
			mime:     mime,
			size:     info.Size(),
			headline: headline(path),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// headline is the document's first Markdown heading, or its file name.
func headline(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // a path under the directory the operator named
	if err != nil {
		return filepath.Base(path)
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "#"); ok {
			return strings.TrimSpace(strings.TrimLeft(after, "#"))
		}
	}
	return filepath.Base(path)
}

// read serves one document, refusing anything outside the served directory.
//
// The URI comes off the wire, so it is checked against the root even though
// every URI a well-behaved client sends is one this server itself advertised.
// A server that trusts a path it was handed is the bug this whole product is
// about.
func read(root string) sdk.ResourceHandler {
	return func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		uri := req.Params.URI
		path, ok := strings.CutPrefix(uri, "file://")
		if !ok {
			return nil, fmt.Errorf("unsupported URI scheme in %q", uri)
		}
		clean := filepath.Clean(path)
		if clean != root && !strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return nil, fmt.Errorf("no such document: %q", uri)
		}
		data, err := os.ReadFile(clean) //nolint:gosec // checked against root immediately above
		if err != nil {
			return nil, fmt.Errorf("no such document: %q", uri)
		}
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
			URI:      uri,
			MIMEType: textExtensions[strings.ToLower(filepath.Ext(clean))],
			Text:     string(data),
		}}}, nil
	}
}
