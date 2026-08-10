package report

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/russellwallace/veritix/internal/audit"
	"github.com/russellwallace/veritix/internal/config"
)

const fixtureDir = "../../testdata/dirty-retail"

// rawValuesInFixture are verbatim contents of the fixture files. None of them
// may appear in a report unless values were explicitly requested.
var rawValuesInFixture = []string{
	"CUS-000001", "CUS-000005", "CUS-999999",
	"alice@example.com", "carol@example.com",
	"Alice Smith", "Frank Green",
	"Zürich", "München", "Montréal",
	"Doohickey", "Widget",
	"Quarterly Sales Report",
}

func run(t *testing.T) *audit.Result {
	t.Helper()
	res, err := audit.Run(t.Context(), audit.Options{
		Paths:  []string{fixtureDir},
		Engine: config.Default().Engine,
	}, nil)
	if err != nil {
		t.Fatalf("audit.Run: %v", err)
	}
	t.Cleanup(func() { _ = res.Close() })
	return res
}

// This is the guarantee the whole product rests on: with a cloud model
// configured, what leaves the process must be counts and shapes, never
// contents. The report is the same boundary — it gets emailed and pasted into
// tickets — so it is tested the same way.
func TestDefaultReportContainsNoRawValues(t *testing.T) {
	res := run(t)

	// HTML matters most here: it is the format that gets emailed.
	for _, format := range []string{"json", "text", "html", "sarif"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			var err error
			switch format {
			case "json":
				err = WriteJSON(&buf, res, "test", Options{Indent: true})
			case "html":
				err = WriteHTML(&buf, res, "test", Options{})
			case "sarif":
				err = WriteSARIF(&buf, res, "test", Options{Indent: true})
			default:
				err = WriteText(&buf, res, Options{})
			}
			if err != nil {
				t.Fatalf("writing %s: %v", format, err)
			}

			out := buf.String()
			if len(out) < 500 {
				t.Fatalf("report is suspiciously short (%d bytes); it may be empty", len(out))
			}
			for _, raw := range rawValuesInFixture {
				if strings.Contains(out, raw) {
					t.Errorf("the default %s report leaks the raw value %q", format, raw)
				}
			}
		})
	}
}

func TestIncludeValuesActuallyIncludesThem(t *testing.T) {
	res := run(t)

	var buf bytes.Buffer
	if err := WriteJSON(&buf, res, "test", Options{IncludeValues: true, Indent: true}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	out := buf.String()

	// Asking for values must actually produce them, or the flag is a lie and
	// nobody can diagnose anything.
	var found int
	for _, raw := range rawValuesInFixture {
		if strings.Contains(out, raw) {
			found++
		}
	}
	if found == 0 {
		t.Error("--include-values produced a report with no values in it")
	}

	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("the report is not valid JSON: %v", err)
	}
	if !doc.Redacted.ValuesIncluded {
		t.Error("the report must record that values were included")
	}
}

// A reader has to be able to tell "nothing notable here" apart from "this was
// withheld from you", or they will draw the wrong conclusion from a clean-
// looking report.
func TestRedactionIsDeclared(t *testing.T) {
	res := run(t)

	var buf bytes.Buffer
	if err := WriteJSON(&buf, res, "test", Options{}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Redacted.ValuesIncluded {
		t.Error("values should not be included by default")
	}
	if doc.Redacted.Note == "" {
		t.Error("a redacted report must say so")
	}
}

func TestReportStructure(t *testing.T) {
	res := run(t)

	var buf bytes.Buffer
	if err := WriteJSON(&buf, res, "test", Options{Indent: true}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if doc.Schema != SchemaVersion {
		t.Errorf("schema = %q, want %q", doc.Schema, SchemaVersion)
	}
	if doc.Dataset.TableCount != len(doc.Tables) {
		t.Errorf("table_count %d does not match the %d tables listed",
			doc.Dataset.TableCount, len(doc.Tables))
	}
	if len(doc.Skipped) == 0 {
		t.Error("the fixture contains unreadable files; they belong in the report")
	}

	// Every table must carry how it was read: a misdetected dialect or
	// encoding invalidates everything else on the page, so a reader needs to
	// be able to check the assumption rather than trust it.
	for _, tb := range doc.Tables {
		if tb.Reading == nil {
			t.Errorf("table %s does not record how it was read", tb.Source)
		}
		if len(tb.Columns) == 0 {
			t.Errorf("table %s has no columns", tb.Source)
		}
	}
}

// Two runs over unchanged files must produce identical reports, or diffs
// between runs are worthless and stored findings will not line up.
func TestReportIsDeterministic(t *testing.T) {
	first := renderOnce(t)
	second := renderOnce(t)

	if first != second {
		t.Error("two runs over the same data produced different reports")
	}
}

func renderOnce(t *testing.T) string {
	t.Helper()
	res := run(t)

	var buf bytes.Buffer
	if err := WriteJSON(&buf, res, "test", Options{Indent: true}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	// The run's own timing varies between runs by design; blank it out so the
	// comparison is about the data.
	var doc Document
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	doc.Run = RunInfo{}
	doc.Dataset.Root = ""

	stable, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(stable)
}

// The HTML report is opened on machines with no network and forwarded to
// people outside the team, so it must not fetch anything from anywhere.
func TestHTMLReportIsSelfContained(t *testing.T) {
	res := run(t)

	var buf bytes.Buffer
	if err := WriteHTML(&buf, res, "test", Options{}); err != nil {
		t.Fatalf("WriteHTML: %v", err)
	}
	out := buf.String()

	for _, external := range []string{"http://", "https://cdn", "<script src", "@import", "url(http"} {
		if strings.Contains(out, external) {
			t.Errorf("the HTML report reaches out to the network: found %q", external)
		}
	}
	for _, want := range []string{"<!doctype html>", "Findings", "</html>"} {
		if !strings.Contains(out, want) {
			t.Errorf("the HTML report is missing %q", want)
		}
	}
}

func TestSARIFStructure(t *testing.T) {
	res := run(t)

	var buf bytes.Buffer
	if err := WriteSARIF(&buf, res, "test", Options{Indent: true}); err != nil {
		t.Fatalf("WriteSARIF: %v", err)
	}

	var log struct {
		Version string `json:"version"`
		Runs    []struct {
			Tool struct {
				Driver struct {
					Name  string `json:"name"`
					Rules []struct {
						ID string `json:"id"`
					} `json:"rules"`
				} `json:"driver"`
			} `json:"tool"`
			Results []struct {
				RuleID string `json:"ruleId"`
				Level  string `json:"level"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("the SARIF output is not valid JSON: %v", err)
	}

	if log.Version != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", log.Version)
	}
	if len(log.Runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(log.Runs))
	}
	r := log.Runs[0]
	if len(r.Results) == 0 {
		t.Fatal("the fixtures produce findings; SARIF should carry them")
	}

	// Every result must reference a rule declared in the catalogue, or
	// consumers will reject the document.
	declared := make(map[string]bool)
	for _, rule := range r.Tool.Driver.Rules {
		declared[rule.ID] = true
	}
	for _, res := range r.Results {
		if !declared[res.RuleID] {
			t.Errorf("result references undeclared rule %q", res.RuleID)
		}
		switch res.Level {
		case "error", "warning", "note":
		default:
			t.Errorf("invalid SARIF level %q", res.Level)
		}
	}
}
