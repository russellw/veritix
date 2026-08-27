package checks_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docPath is the reference list of every rule a deterministic audit can
// report.
const docPath = "../../docs/checks.md"

// ruleLiteral matches a rule id where a finding is given one: either directly,
// as Rule: "column.empty", or as an observation code that checkStructure
// promotes to a finding under its own name.
var ruleLiteral = regexp.MustCompile(`(?:Rule|Code):\s*"([a-z][a-z0-9]*\.[a-z0-9_]+)"`)

// docLiteral matches a rule id as the documentation writes one, in backticks.
// Only the families a finding actually uses count, so that `orders.csv` or
// `rules.Evaluate` in the prose is not read as a rule.
var docLiteral = regexp.MustCompile("`([a-z][a-z0-9]*\\.[a-z0-9_]+)`")

var ruleFamilies = map[string]bool{
	"column": true, "table": true, "key": true, "reference": true,
	"domain": true, "csv": true, "excel": true, "ingest": true, "rule": true,
}

// TestEveryRuleIsDocumented holds docs/checks.md to the code, in both
// directions.
//
// A reference list that has drifted from the code is worse than no reference
// list, because somebody believes it: a rule missing from the list reads as a
// check that does not exist, and a rule listed but removed reads as coverage
// that is no longer there. Neither is visible by reading either file alone,
// which is why this is a test rather than a note asking people to remember.
func TestEveryRuleIsDocumented(t *testing.T) {
	inCode := rulesInCode(t)
	if len(inCode) < 40 {
		t.Fatalf("found only %d rule ids in the source; the scan is broken, not the docs", len(inCode))
	}

	doc, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docPath, err)
	}
	inDoc := make(map[string]bool)
	for _, m := range docLiteral.FindAllStringSubmatch(string(doc), -1) {
		id := m[1]
		if ruleFamilies[strings.SplitN(id, ".", 2)[0]] {
			inDoc[id] = true
		}
	}

	for _, id := range sortedKeys(inCode) {
		if !inDoc[id] {
			t.Errorf("%s reports %q and %s does not list it", inCode[id], id, docPath)
		}
	}
	for _, id := range sortedKeys(inDoc) {
		if _, ok := inCode[id]; !ok {
			t.Errorf("%s lists %q, which nothing in internal/ reports", docPath, id)
		}
	}
}

// rulesInCode returns every rule id a finding can carry, mapped to the file it
// came from. Tests are excluded: a fixture's invented rule id is not a rule.
func rulesInCode(t *testing.T) map[string]string {
	t.Helper()
	found := make(map[string]string)
	err := filepath.WalkDir("../../internal", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path) //nolint:gosec // a path from a walk of this repo
		if err != nil {
			return err
		}
		for _, m := range ruleLiteral.FindAllStringSubmatch(string(b), -1) {
			if !ruleFamilies[strings.SplitN(m[1], ".", 2)[0]] {
				continue
			}
			if _, seen := found[m[1]]; !seen {
				found[m[1]] = filepath.ToSlash(path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the source: %v", err)
	}
	return found
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
