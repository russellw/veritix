package runs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/russellw/veritix/internal/rules"
)

// Everything under the data directory is named by an id the store generated.
// The check is here rather than at each caller because this package is where
// the layout of that directory is decided, and a path built from a request is
// exactly the kind of thing that acquires a second caller later.
func TestDatasetPathsRefuseAnythingThatIsNotAnID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "..", "../../etc", "a/b", `a\b`, "..%2f"} {
		if _, err := DatasetRulesPath(dir, id); err == nil {
			t.Errorf("DatasetRulesPath accepted %q", id)
		}
		if _, err := AcceptedRules(dir, id); err == nil {
			t.Errorf("AcceptedRules accepted %q", id)
		}
	}

	path, err := DatasetRulesPath(dir, "0198f3c1-2f42-7c8e-9a1b-2c3d4e5f6071")
	if err != nil {
		t.Fatalf("a real id was refused: %v", err)
	}
	if filepath.Dir(filepath.Dir(path)) != filepath.Join(dir, "datasets") {
		t.Errorf("the rules file landed at %s", path)
	}
}

// A dataset with no accepted rules is the normal case and is not an error: it
// is the state every dataset starts in.
func TestAcceptedRulesIsNilBeforeAnythingIsAccepted(t *testing.T) {
	dir := t.TempDir()
	id := "0198f3c1-2f42-7c8e-9a1b-2c3d4e5f6071"

	f, err := AcceptedRules(dir, id)
	if err != nil || f != nil {
		t.Fatalf("AcceptedRules = %v, %v; want nil, nil", f, err)
	}

	path, err := DatasetRulesPath(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	body := "version: 1\nrules:\n  - id: a\n    table: t\n    column: c\n    expect: not_null\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err = AcceptedRules(dir, id); err != nil || f == nil || len(f.Rules) != 1 {
		t.Fatalf("AcceptedRules = %v, %v", f, err)
	}
}

// The customer's own rules file and the rules they accepted are additive, and
// a collision between them is refused rather than resolved: two rules of one
// name would report under one id and mean two different things.
func TestMergeIsAdditiveAndRefusesACollision(t *testing.T) {
	own := &rules.File{Version: 1, Rules: []rules.Rule{
		{ID: "amount_positive", Table: "orders", Column: "amount", Expect: rules.ExpectPositive},
	}}
	accepted := &rules.File{Version: 1, Rules: []rules.Rule{
		{ID: "status_domain", Table: "customers", Column: "status",
			Expect: rules.ExpectOneOf, Values: []string{"a"}},
	}}

	merged, err := Merge(own, accepted)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(merged.Rules) != 2 {
		t.Errorf("merged %d rules, want 2", len(merged.Rules))
	}

	if f, err := Merge(nil, nil); f != nil || err != nil {
		t.Errorf("Merge of nothing = %v, %v; want nil, nil", f, err)
	}
	if _, err := Merge(own, own); err == nil {
		t.Error("two rules of the same name were merged")
	}
}
