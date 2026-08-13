package redact

import (
	"errors"
	"strings"
	"testing"
)

func TestShapeDisclosesNothing(t *testing.T) {
	g := New(Policy{})

	cases := map[string]string{
		"CUS-004417":           "⟨XXX-999999⟩",
		"alice@example.com":    "⟨XXXXX@XXXXXXX.XXX⟩",
		"£1,234.50":            "⟨£9,999.99⟩",
		"2024-03-04":           "⟨9999-99-99⟩",
		"Acme Ltd":             "⟨XXXX XXX⟩",
		"Ünïcode":              "⟨XXXXXXX⟩",
		"O'Brien\tCorporation": "⟨X'XXXXX XXXXXXXXXXX⟩",
		"":                     "⟨⟩",
		"03/04/2024 09:15:00":  "⟨99/99/9999 99:99:99⟩",
	}
	for in, want := range cases {
		if got := g.Shape(in).String(); got != want {
			t.Errorf("Shape(%q) = %q, want %q", in, got, want)
		}
	}
}

// The delimiters are the whole defense against a shape being read as data, so
// nothing may reach the model wearing a shape's clothes without them, and a
// sentinel — which is a real value from a fixed vocabulary — must not wear
// them at all.
func TestAShapeIsAlwaysDelimitedAndASentinelNever(t *testing.T) {
	g := New(Policy{})

	marked := []string{
		g.Shape("CUS-004417").String(),
		g.Value("CUS-004417").String(),
		g.Derived("XXX-999999").String(),
		g.Cell("Acme", true).(Text).String(),
		g.Cell(1234.50, false).(Text).String(),
		g.Shape(strings.Repeat("a", 500)).String(), // truncated, and still closed
	}
	for _, got := range marked {
		if !strings.HasPrefix(got, "⟨") || !strings.HasSuffix(got, "⟩") {
			t.Errorf("a shape reached the model undelimited: %q", got)
		}
		if Unmark(got) == got {
			t.Errorf("Unmark did not recover the bare shape of %q", got)
		}
	}

	if got := g.Sentinel("n/a").String(); got != "n/a" {
		t.Errorf(`Sentinel("n/a") = %q: a placeholder is a value, not a shape`, got)
	}

	// A value permitted by policy is data, and data is never bracketed.
	allowed := New(Policy{AllowValues: true}).Value("Acme Ltd").String()
	if strings.ContainsAny(allowed, "⟨⟩") {
		t.Errorf("an allowed value was dressed as a shape: %q", allowed)
	}
}

// The default policy is the product's promise. A value must not survive it in
// any form a reader could recover.
func TestDefaultPolicyShapesEveryValue(t *testing.T) {
	g := New(Policy{})

	for _, raw := range []string{"alice@example.com", "Acme Ltd", "CUS-004417", "89.99"} {
		got := g.Value(raw).String()
		if strings.Contains(got, raw) {
			t.Errorf("Value(%q) returned the value itself: %q", raw, got)
		}
		if bare := Unmark(got); len([]rune(bare)) != len([]rune(raw)) && raw != "" {
			t.Errorf("Value(%q) = %q: a shape should be the same length as its value", raw, got)
		}
	}
	if s := g.Stats(); s.Shaped != 4 || s.Passed != 0 {
		t.Errorf("stats = %+v, want 4 shaped and nothing passed", s)
	}
}

func TestAllowedValuesAreStillMasked(t *testing.T) {
	g := New(Policy{AllowValues: true})

	cases := map[string]string{
		"Acme Ltd":                      "Acme Ltd",
		"contact alice@example.com now": "contact [email] now",
		"4111 1111 1111 1111":           "[long-number]",
		"123-45-6789":                   "[national-id]",
	}
	for in, want := range cases {
		if got := g.Value(in).String(); got != want {
			t.Errorf("Value(%q) = %q, want %q", in, got, want)
		}
	}

	long := strings.Repeat("x", 500)
	if got := g.Value(long).String(); len(got) > 130 {
		t.Errorf("a %d-character value was allowed through whole", len(got))
	}
}

// Numbers are values too: only a cell the caller can vouch for as an aggregate
// leaves as a number.
func TestCellShapesEverythingButAggregates(t *testing.T) {
	g := New(Policy{})

	if got := g.Cell(int64(42), true); got != int64(42) {
		t.Errorf("an aggregate count came back as %v", got)
	}
	got := g.Cell(1234.50, false)
	txt, ok := got.(Text)
	if !ok {
		t.Fatalf("a non-aggregate number came back as %T, want Text", got)
	}
	if txt.String() != "⟨9999.9⟩" {
		t.Errorf("Cell(1234.50) = %q, want its shape", txt.String())
	}
	if s := g.Cell("Acme", true); s.(Text).String() != "⟨XXXX⟩" {
		t.Errorf("an aggregate over text must still be treated as a value, got %v", s)
	}
	if v := g.Cell(nil, false); v != nil {
		t.Errorf("Cell(nil) = %v, want nil", v)
	}
}

// DuckDB quotes the offending value in its error messages, so an error is a
// way for a cell to escape without any tool deciding to send it.
func TestEngineErrorsKeepTheDiagnosisAndDropTheValue(t *testing.T) {
	g := New(Policy{})

	err := errors.New(`Conversion Error: Could not convert string 'N/A' to INT32 in column "amount"`)
	got := g.EngineError(err, "SELECT sum(amount) FROM orders").String()

	if strings.Contains(got, "N/A") {
		t.Errorf("the error still carries the cell value: %q", got)
	}
	for _, keep := range []string{"Conversion Error", "Could not convert", `"amount"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("the error lost the part the model needs (%q): %q", keep, got)
		}
	}
}

// DuckDB echoes the offending statement back inside its error message, so
// scrubbing every quoted run rewrites the model's own SQL in front of it.
// qwen3.5-35b sent REPLACE(amount, ',', ”) and was shown REPLACE(amount, '⟨,⟩',
// '⟨⟩'), concluded the engine was mangling literals — it said so — and stopped
// writing SQL. Text the model sent cannot be disclosed by sending it back.
func TestAnErrorDoesNotRewriteTheModelsOwnSQL(t *testing.T) {
	g := New(Policy{})

	sql := `SELECT count(*) FROM orders WHERE substring(amount, 1, 1) = '-' ` +
		`AND REPLACE(amount, ',', '') <> 'N/A'`
	err := errors.New(`Binder Error: no function REPLACE(VARCHAR, VARCHAR)` +
		"\n" + `LINE 1: ... = '-' AND REPLACE(amount, ',', '') <> 'N/A'`)

	got := g.EngineError(err, sql).String()

	// Every literal here is the model's own, quoted back at it.
	for _, keep := range []string{`'-'`, `','`, `''`, `'N/A'`} {
		if !strings.Contains(got, keep) {
			t.Errorf("the model's own literal %s was rewritten: %q", keep, got)
		}
	}
	if strings.ContainsAny(got, "⟨⟩") {
		t.Errorf("a shape was substituted into the model's own statement: %q", got)
	}

	// The same value, when it is not something the model sent, is still shaped.
	other := g.EngineError(
		errors.New(`Conversion Error: Could not convert string 'CUS-004417' to INT`),
		sql).String()
	if strings.Contains(other, "CUS-004417") {
		t.Errorf("a cell value the model never sent escaped through an error: %q", other)
	}
	if !strings.Contains(other, "⟨XXX-999999⟩") {
		t.Errorf("the escaped value was not shaped: %q", other)
	}
}

func TestSealRefusesUnclassifiedContent(t *testing.T) {
	g := New(Policy{})

	// What a tool result looks like: names and prose as plain strings, values
	// as Text, numbers as numbers.
	ok := struct {
		Table  string `json:"table"`
		Sample []Text `json:"sample"`
		Rows   int64  `json:"rows"`
	}{"orders", g.Values([]string{"Acme"}), 12}
	sealed, err := g.Seal(ok)
	if err != nil {
		t.Fatalf("a well-formed result would not seal: %v", err)
	}
	if !strings.Contains(sealed.String(), "XXXX") {
		t.Errorf("the sealed result lost its shaped value: %s", sealed)
	}

	// A result set straight out of the engine: [][]any, where a cell's type no
	// longer says what it holds. This is the shape that would leak, and it is
	// the one the walk exists to catch.
	leaky := struct {
		Rows [][]any `json:"rows"`
	}{[][]any{{"alice@example.com", int64(3)}}}
	if _, err := g.Seal(leaky); err == nil {
		t.Fatal("Seal accepted a raw query result")
	} else if !strings.Contains(err.Error(), "Rows[0][0]") {
		t.Errorf("the refusal should name the offending field, got: %v", err)
	}

	// The same rows, once the cells have been through the guard.
	cleared := struct {
		Rows [][]any `json:"rows"`
	}{[][]any{{g.Cell("alice@example.com", false), g.Cell(int64(3), true)}}}
	if _, err := g.Seal(cleared); err != nil {
		t.Errorf("cleared cells would not seal: %v", err)
	}

	if _, err := g.Seal(struct{ B []byte }{[]byte("secret")}); err == nil {
		t.Error("Seal accepted raw bytes")
	}
}
