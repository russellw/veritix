package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/agent/llm/llmtest"
	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/telemetry"
)

const fixtureDir = "../../testdata/dirty-retail"

// rawValuesInFixture are verbatim contents of the fixture files, the same list
// internal/report and internal/agent assert on.
var rawValuesInFixture = []string{
	"CUS-000001", "CUS-000005", "CUS-999999",
	"alice@example.com", "carol@example.com",
	"Alice Smith", "Frank Green",
	"Zürich", "München", "Montréal",
	"Doohickey", "Widget",
	"Quarterly Sales Report",
}

// schemaInFixture is the customer's own vocabulary: the names of their files
// and their columns. A report carries these and so does the interface, because
// a person reading them already has the data. A span does not, because a span
// is an access log that leaves the machine for somebody else's collector, and
// the shape of a company's exports is itself commercially sensitive.
//
// This is the half of the policy that is easy to lose. Nobody would put a cell
// value in a span attribute on purpose; putting the table name in one is the
// obvious, helpful, wrong thing to do.
var schemaInFixture = []string{
	"customers.csv", "orders.csv", "regions.csv", "sales.xlsx",
	"customers_csv", "orders_csv", "regions_csv",
	"customer_id", "signup_date", "region_code", "order_id", "amount",
}

// TestNoSpanCarriesCustomerData audits a real fixture with a recording
// exporter installed and reads back every span that would have been exported.
//
// It is the same shape of test as report's TestDefaultReportContainsNoRawValues
// and agent's scan of outbound payloads, for the same reason: the promise is
// about what leaves the process, so the test has to look at what left rather
// than at the code that decided.
func TestNoSpanCarriesCustomerData(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(otel.GetTracerProvider()) })

	// A scripted model, driven through the real loop, so the agent's spans are
	// in the sample too. That is where the risk actually is: the deterministic
	// stages count things, while the agent handles the model's SQL, its prose
	// and the tool results, and any of those in a span attribute would be the
	// leak this test exists to catch.
	model := llmtest.New(
		llmtest.Turn{Calls: []llmtest.Call{{
			ID: "c1", Name: "run_sql",
			Input: map[string]any{"sql": "SELECT count(*) FROM customers_csv WHERE region_code = 'EMEA'"},
		}}},
		llmtest.Turn{Calls: []llmtest.Call{{
			ID: "c2", Name: "describe_table",
			Input: map[string]any{"table": "customers.csv"},
		}}},
		llmtest.Turn{Text: "Nothing further."},
	)

	ctx := context.Background()
	res, err := audit.Run(ctx, audit.Options{
		Paths:   []string{fixtureDir},
		Engine:  config.Default().Engine,
		Profile: profile.Options{},
		Agent:   &agent.Options{Provider: model, MaxSteps: 5},
	}, nil)
	if err != nil {
		t.Fatalf("auditing the fixture: %v", err)
	}
	defer func() { _ = res.Close() }()

	if err := tp.ForceFlush(ctx); err != nil {
		t.Fatalf("flushing spans: %v", err)
	}

	spans := rec.Ended()
	if len(spans) == 0 {
		t.Fatal("the audit produced no spans at all, so this test proves nothing")
	}

	// A stage that stopped producing a span would make this test pass by
	// measuring nothing, so name what has to be there.
	want := []string{"audit.run", "audit.discover", "audit.ingest", "audit.profile",
		"audit.checks", "audit.rules", "audit.verify", "audit.agent",
		"agent.step", "agent.tool"}
	seen := make(map[string]bool, len(spans))
	for _, s := range spans {
		seen[s.Name()] = true
	}
	for _, w := range want {
		if !seen[w] {
			t.Errorf("no %s span was recorded", w)
		}
	}

	forbidden := append(append([]string(nil), rawValuesInFixture...), schemaInFixture...)
	for _, s := range spans {
		// Everything a span can carry as text: its name, its attributes, its
		// status, and the events and links hung off it.
		var texts []string
		texts = append(texts, s.Name(), s.Status().Description)
		for _, a := range s.Attributes() {
			texts = append(texts, string(a.Key), a.Value.String())
		}
		for _, ev := range s.Events() {
			texts = append(texts, ev.Name)
			for _, a := range ev.Attributes {
				texts = append(texts, string(a.Key), a.Value.String())
			}
		}
		for _, a := range s.Resource().Attributes() {
			texts = append(texts, string(a.Key), a.Value.String())
		}

		for _, text := range texts {
			for _, bad := range forbidden {
				if strings.Contains(text, bad) {
					t.Errorf("span %q carries %q, which is the customer's data: %q",
						s.Name(), bad, text)
				}
			}
		}
	}
}

// TestNothingIsExportedWithoutBeingAskedTo pins the default. An OTLP endpoint
// is a network egress from a process holding data the customer declined to
// send to a vendor, so it has to be off unless somebody turned it on.
func TestNothingIsExportedWithoutBeingAskedTo(t *testing.T) {
	if config.Default().OTel.Enabled {
		t.Error("OpenTelemetry export is on by default")
	}

	tel, err := telemetry.Start(context.Background(), telemetry.Options{Enabled: false, Endpoint: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatalf("starting with export off: %v", err)
	}
	if !tel.Disabled {
		t.Error("export off still built exporters")
	}
	if err := tel.Shutdown(context.Background()); err != nil {
		t.Errorf("shutting down a disabled telemetry: %v", err)
	}
}

// TestEnablingActuallyExports is the other half: off must export nothing, and
// on must export something. A telemetry setup that silently sends nowhere is
// the failure mode that survives every review, because the code reads
// correctly and the collector is somebody else's problem.
func TestEnablingActuallyExports(t *testing.T) {
	got := make(chan string, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.URL.Path:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	ctx := context.Background()
	tel, err := telemetry.Start(ctx, telemetry.Options{
		Enabled:       true,
		Endpoint:      collector.URL,
		ServiceName:   "veritix-test",
		Version:       "test",
		ExportTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("starting the exporters: %v", err)
	}

	_, span := telemetry.Tracer().Start(ctx, "audit.run")
	span.End()

	// Shutdown flushes, so by the time it returns the span has been sent or
	// the attempt has failed. Either way there is nothing left to wait for.
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	select {
	case path := <-got:
		if path != "/v1/traces" {
			t.Errorf("exported to %q, want /v1/traces", path)
		}
	default:
		t.Error("nothing reached the collector")
	}
}
