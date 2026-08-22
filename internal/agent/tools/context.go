package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/russellw/veritix/internal/agent/llm"
	"github.com/russellw/veritix/internal/agent/redact"
	"github.com/russellw/veritix/internal/mcpclient"
)

// The two tools in this file are the only ones that reach outside the process,
// and they are the narrowest surface that does the job. The model names a
// document by an id out of a catalog Veritix built; Veritix looks the id up
// and sends the *catalog's* URI. Nothing the model wrote leaves. That is the
// same rule the rest of this package follows for table and column names, and
// internal/mcpclient's package comment is the whole argument.
//
// They exist at all only when a context server is configured, so a default
// install offers a model exactly the tool set it offered before M5b — which
// also keeps the cached prompt prefix identical for everybody who has not
// turned this on.

func listContext() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "list_context",
			Description: "List the customer's own documents that are available to read — " +
				"data dictionaries, warehouse catalogs, tickets. The brief already lists " +
				"them, so this is for confirming an id rather than for discovering one.",
		},
		invoke: func(_ context.Context, w *World, _ json.RawMessage) (any, error) {
			if w.Context == nil {
				return nil, errors.New("no context documents are configured for this run")
			}
			return struct {
				Documents []mcpclient.Document `json:"documents"`
			}{w.Context.Catalog()}, nil
		},
	}
}

func readContext() *Tool {
	return &Tool{
		Definition: llm.Tool{
			Name: "read_context",
			Description: "Read one of the customer's own documents by its id. Use it when a " +
				"column's meaning decides whether something is a defect: what a code " +
				"column is allowed to contain, whether a number is cumulative or per " +
				"period, how one file's identifier joins to another's. A document says " +
				"what should be true; the data is where you find out whether it is, so " +
				"follow a reading with the query that tests it.",
			Properties: map[string]any{
				"id": str("the document id, as listed in the brief or by list_context"),
			},
			Required: []string{"id"},
		},
		invoke: func(ctx context.Context, w *World, args json.RawMessage) (any, error) {
			if w.Context == nil {
				return nil, errors.New("no context documents are configured for this run")
			}
			var in struct {
				ID string `json:"id"`
			}
			if err := decode(args, &in); err != nil {
				return nil, err
			}

			doc, err := w.Context.Read(ctx, in.ID)
			if errors.Is(err, mcpclient.ErrUnknown) {
				// Named rather than guessed: the ids are short and the catalog
				// is small, so the correction that actually helps is the list
				// itself.
				return nil, fmt.Errorf("%w. The documents are: %s", err, catalogIDs(w.Context))
			}
			if err != nil {
				return nil, fmt.Errorf("that document could not be read: %w", err)
			}

			out := struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Server string `json:"server"`
				// Text is the document as the customer's own system returned
				// it. Guard.Document is what admits it, and it is the one
				// thing in a tool result that has not been redacted — see that
				// method for whose decision that is.
				Text redact.Text `json:"text"`
				Note string      `json:"note,omitempty"`
			}{
				ID:     doc.ID,
				Name:   doc.Name,
				Server: doc.Server,
				Text:   w.Guard.Document(doc.Text),
			}
			if doc.Truncated {
				out.Note = "this document was longer than the per-document limit and is cut off here"
			}
			return out, nil
		},
	}
}

// catalogIDs renders the available ids for a correction.
func catalogIDs(lib *mcpclient.Library) string {
	docs := lib.Catalog()
	if len(docs) == 0 {
		return "(none)"
	}
	out := ""
	for i, d := range docs {
		if i > 0 {
			out += ", "
		}
		out += d.ID
	}
	return out
}
