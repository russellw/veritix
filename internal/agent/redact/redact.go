// Package redact is the single path from Veritix's process to a language
// model.
//
// The product's premise is that commercially sensitive data never leaves the
// customer's machine. An agentic auditor puts that promise under the most
// pressure it will ever face: the model has to understand the data well enough
// to find problems in it, and the obvious way to make it understand is to show
// it the data. So the guard exists to make the useful thing and the safe thing
// the same thing.
//
// The default policy sends schema, counts, ratios, and derived *shapes*:
// "CUS-004417" leaves as "⟨XXX-999999⟩", which is precise enough to reason
// about and useless to anyone who intercepts it. Raw values leave only when the
// operator passes --allow-sample-values, and even then they are masked and
// truncated first.
//
// # How it is enforced
//
// Two types carry the rule, so that a tool added later cannot leak by
// forgetting to call anything:
//
//   - [Text] is the only string type that may hold customer-derived content,
//     and the only way to make one is a Guard method that applies the policy.
//   - [Sealed] is the only thing the agent loop will put in a message to the
//     model, and the only way to make one is [Guard.Seal], which refuses to
//     serialize a value carrying unclassified content.
//
// A new tool that returns a struct full of raw strings does not compile into a
// leak: it fails to seal, at the point where the result would have been sent.
//
// # What it does not claim
//
// The guard bounds what Veritix *sends*. It is not a defense against a model
// that is actively trying to smuggle data out through the channel it is given:
// aggregates can encode values if you choose them carefully enough, and any
// tool surface rich enough to audit a dataset is rich enough to do that
// slowly. The guarantee is that ordinary operation discloses no cell values,
// and that everything sent is recorded in the run's trace, where a customer
// can read exactly what left.
package redact

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
)

// Policy says what may leave the process.
type Policy struct {
	// AllowValues permits raw cell values, masked and truncated, instead of
	// shapes. It is off unless the operator turns it on for a run.
	AllowValues bool
	// MaxValueLen truncates a value that is allowed through. Zero picks a
	// default.
	MaxValueLen int
	// MaxShapeLen truncates a shape. A shape longer than this describes free
	// text, where the shape is neither informative nor entirely safe.
	MaxShapeLen int
}

func (p Policy) withDefaults() Policy {
	if p.MaxValueLen <= 0 {
		p.MaxValueLen = 120
	}
	if p.MaxShapeLen <= 0 {
		p.MaxShapeLen = 60
	}
	return p
}

// Guard applies a policy and counts what it did, so that a run can report how
// much was withheld rather than asking anybody to take it on trust.
type Guard struct {
	policy Policy

	mu    sync.Mutex
	stats Stats
}

// Stats is what the guard did during a run.
type Stats struct {
	// Shaped is how many values were replaced by their shape.
	Shaped int `json:"shaped"`
	// Masked is how many values were allowed through with identifying
	// substrings removed.
	Masked int `json:"masked"`
	// Passed is how many values were allowed through unchanged.
	Passed int `json:"passed"`
	// Truncated is how many were cut for length.
	Truncated int `json:"truncated"`
	// Sealed is how many tool results were serialized for the model.
	Sealed int `json:"sealed"`
	// Bytes is how many bytes of tool results were sent.
	Bytes int `json:"bytes"`
}

// New returns a guard applying the policy.
func New(p Policy) *Guard { return &Guard{policy: p.withDefaults()} }

// Policy reports the policy in force, for the trace and the report.
func (g *Guard) Policy() Policy { return g.policy }

// Stats reports what the guard has done so far.
func (g *Guard) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stats
}

// Text is customer-derived content that has been through the guard.
//
// Its contents are unexported and it has no constructor: outside this package
// the only way to obtain one is from a Guard, which is what makes "every value
// that reaches the model went through the policy" a property of the type
// system rather than of everyone's diligence.
type Text struct{ s string }

// MarshalJSON renders the cleared text.
func (t Text) MarshalJSON() ([]byte, error) { return json.Marshal(t.s) }

// String returns the cleared text.
func (t Text) String() string { return t.s }

// Value clears one customer-derived value under the policy.
func (g *Guard) Value(s string) Text {
	if !g.policy.AllowValues {
		return g.Shape(s)
	}

	out, masked := mask(s)
	truncated := false
	if len(out) > g.policy.MaxValueLen {
		out = out[:g.policy.MaxValueLen] + "…"
		truncated = true
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if masked {
		g.stats.Masked++
	} else {
		g.stats.Passed++
	}
	if truncated {
		g.stats.Truncated++
	}
	return Text{out}
}

// Shape replaces a value by its pattern regardless of policy. Digits become 9
// and letters become X, character for character, which is the same
// transformation the profiler applies in SQL: the two have to agree, or a
// shape in a tool result would not match the shape in the profile the model
// was given alongside it.
//
// The result is delimited — see [Mark].
func (g *Guard) Shape(s string) Text {
	out := shape(s)
	truncated := false
	if len(out) > g.policy.MaxShapeLen {
		out = out[:g.policy.MaxShapeLen] + "…"
		truncated = true
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.Shaped++
	if truncated {
		g.stats.Truncated++
	}
	return Text{Mark(out)}
}

// The delimiters a shape is wrapped in before it is sent.
//
// U+27E8/U+27E9 rather than "<>": Go's JSON encoder escapes the ASCII pair to
// < and >, which would put a shape in the trace in a form no reader
// recognizes, and the angle brackets do occur in real exports.
const (
	shapeOpen  = "⟨"
	shapeClose = "⟩"
)

// Mark delimits a shape so that it cannot be read as a cell value.
//
// A shape sits in a tool result exactly where a value would sit, and looks
// like one: "XXXX" is a plausible region code and "99.99" a plausible price.
// Two models eight times apart in size both read them as contents and queried
// for them — the larger spent its last seven steps on WHERE region = 'XXXX',
// which correctly matched nothing every time. Telling the model in the system
// prompt what a shape is does not survive twenty steps of a filling context;
// making a shape not look like a value survives everything, because it travels
// with the shape.
//
// So this is a property of the representation rather than a rule about a
// mistake: ⟨XXX-999999⟩ pasted into SQL is still wrong, but it is visibly
// wrong, in the model's own output, at the moment it writes it.
func Mark(shape string) string { return shapeOpen + shape + shapeClose }

// Unmark removes the delimiters, for a caller that needs the bare pattern
// back — the profiler's shapes are stored and reported undelimited.
func Unmark(s string) string {
	return strings.TrimSuffix(strings.TrimPrefix(s, shapeOpen), shapeClose)
}

// Derived wraps a shape the profiler already derived, and which therefore
// contains nothing to withhold.
//
// It exists so the counters keep meaning something. Shapes are fixed points of
// the shape function, so passing them through [Guard.Shape] would be harmless
// but would report every summary in a profile as a value that had to be
// redacted, and a customer reading "4,812 values withheld" deserves that number
// to be the count of values that were actually withheld.
//
// It is delimited like any other shape: a shape reaching the model from the
// profile and one reaching it from a query have to be the same kind of thing,
// or the distinction is one more piece of lore for the model to lose.
func (g *Guard) Derived(s string) Text { return Text{Mark(s)} }

// Sentinel wraps a recognized placeholder token — "n/a", "-", "unknown" —
// which the profiler matched against a fixed vocabulary.
//
// Unlike a shape this is a literal cell value, and it is deliberately *not*
// delimited: it is genuinely in the data, WHERE status = 'n/a' is a query
// worth writing, and disguising it as a shape would cost the model the one
// class of value it is allowed to know verbatim. Nothing customer-specific
// can arrive here, because a token that is not in the vocabulary is not a
// sentinel.
func (g *Guard) Sentinel(s string) Text { return Text{s} }

// Values clears a list of values.
func (g *Guard) Values(ss []string) []Text {
	out := make([]Text, len(ss))
	for i, s := range ss {
		out[i] = g.Value(s)
	}
	return out
}

// Cell clears one cell of a query result.
//
// A number is a value like any other — a salary is as sensitive as a name — so
// only cells the caller can vouch for as aggregates pass through as numbers.
// Everything else is shaped, which for a number means its magnitude and
// precision survive and its value does not: 1234.50 leaves as 9999.99.
func (g *Guard) Cell(v any, aggregate bool) any {
	switch t := v.(type) {
	case nil:
		return nil
	case bool:
		return t
	case time.Time:
		if aggregate {
			return t.Format(time.RFC3339)
		}
		return g.Shape(t.Format(time.RFC3339))
	case string:
		// Text is a value whether or not an aggregate produced it: max(name)
		// and string_agg(email) are cell contents, not statistics.
		return g.Value(t)
	}

	if aggregate && isNumber(v) {
		g.mu.Lock()
		g.stats.Passed++
		g.mu.Unlock()
		return v
	}
	return g.Shape(fmt.Sprint(v))
}

func isNumber(v any) bool {
	switch reflect.ValueOf(v).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

// EngineError clears a database error before showing it to the model.
//
// The model needs to see why its SQL failed or it cannot fix it, but DuckDB
// quotes the offending value in its message — "Could not convert string 'N/A'
// to INT" — so an error is a way for a cell value to escape without any tool
// having decided to send it. Single-quoted content is therefore shaped and
// everything else, including table and column names, is kept.
//
// Except what the model already has. sql is the statement that produced the
// error, and a quoted literal that appears in it verbatim is passed through
// unchanged, because text the model sent cannot be disclosed to the model by
// returning it. That exception is not a nicety. DuckDB echoes the offending
// statement back inside the message, so without it every literal in the
// model's own query comes back rewritten: qwen3.5-35b sent
// REPLACE(amount, ',', ”) and was shown REPLACE(amount, '⟨,⟩', '⟨⟩'),
// concluded the engine was mangling its literals — it said so, in as many
// words — and gave up on SQL for the rest of the run.
//
// A cell value that happens to appear in the model's query is passed through
// too, and that is correct: the model wrote it, so it already knew it.
func (g *Guard) EngineError(err error, sql string) Text {
	if err == nil {
		return Text{}
	}
	return Text{g.scrubQuoted(err.Error(), sql)}
}

func (g *Guard) scrubQuoted(s, sql string) string {
	var b strings.Builder
	rest := s
	for {
		i := strings.IndexByte(rest, '\'')
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		j := strings.IndexByte(rest[i+1:], '\'')
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i+1])
		quoted := rest[i+1 : i+1+j]
		if sql != "" && strings.Contains(sql, "'"+quoted+"'") {
			b.WriteString(quoted)
		} else {
			b.WriteString(g.Shape(quoted).String())
		}
		b.WriteByte('\'')
		rest = rest[i+j+2:]
	}
}

// Sealed is a tool result cleared for egress.
//
// The agent loop will only send one of these, and [Guard.Seal] is the only way
// to make one, so a tool result reaches the model exactly when the guard has
// inspected it.
type Sealed struct{ b []byte }

// Bytes returns the JSON that will be sent.
func (s Sealed) Bytes() []byte { return s.b }

// String returns the JSON that will be sent.
func (s Sealed) String() string { return string(s.b) }

// Len is the size of the payload.
func (s Sealed) Len() int { return len(s.b) }

// Seal serializes a tool result for the model, refusing anything that carries
// content the guard has not cleared.
//
// The check is structural: a string reached through an `any` — a query result
// cell, a decoded JSON value, anything whose type stopped saying what it holds
// — is unclassified, and unclassified content does not leave. Tools pass those
// through [Guard.Cell] or [Guard.Value] first, which is the difference between
// a value that has been considered and one that merely happened to be there.
// A plain string field on a result struct is metadata the tool author wrote or
// a name out of the schema, and passes.
func (g *Guard) Seal(v any) (Sealed, error) {
	if err := inspect(reflect.ValueOf(v), false, ""); err != nil {
		return Sealed{}, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return Sealed{}, fmt.Errorf("redact: sealing a tool result: %w", err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.Sealed++
	g.stats.Bytes += len(b)
	return Sealed{b}, nil
}

// SealText wraps a message the tool wrote itself. It is for prose Veritix
// authored — "no such table" — and not a way round the walk.
func (g *Guard) SealText(format string, args ...any) Sealed {
	msg := struct {
		Message string `json:"message"`
	}{fmt.Sprintf(format, args...)}
	b, err := json.Marshal(msg)
	if err != nil { // a string cannot fail to marshal
		b = []byte(`{"message":"the result could not be encoded"}`)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	g.stats.Sealed++
	g.stats.Bytes += len(b)
	return Sealed{b}
}

var (
	textType = reflect.TypeOf(Text{})
	timeType = reflect.TypeOf(time.Time{})
)

// inspect walks a value looking for content the guard has not seen. The path
// is carried so that a refusal names the field a developer has to fix.
func inspect(v reflect.Value, viaAny bool, path string) error {
	if !v.IsValid() {
		return nil
	}
	switch v.Type() {
	case textType, timeType:
		return nil
	}

	switch v.Kind() {
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return inspect(v.Elem(), true, path)

	case reflect.String:
		if viaAny {
			return fmt.Errorf(
				"redact: refusing to send an unclassified string at %s: "+
					"pass customer-derived values through Guard.Value or Guard.Cell",
				pathOr(path))
		}
		return nil

	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return inspect(v.Elem(), viaAny, path)

	case reflect.Struct:
		t := v.Type()
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			if err := inspect(v.Field(i), viaAny, path+"."+f.Name); err != nil {
				return err
			}
		}
		return nil

	case reflect.Slice, reflect.Array:
		// Bytes are opaque: nothing can tell whether they are a value, and a
		// tool that wants to send text has better ways to say so.
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("redact: refusing to send raw bytes at %s", pathOr(path))
		}
		for i := range v.Len() {
			if err := inspect(v.Index(i), viaAny, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		return nil

	case reflect.Map:
		for _, k := range v.MapKeys() {
			if err := inspect(v.MapIndex(k), viaAny, fmt.Sprintf("%s[%v]", path, k)); err != nil {
				return err
			}
		}
		return nil

	case reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return fmt.Errorf("redact: %s cannot be serialized at %s", v.Kind(), pathOr(path))

	default:
		return nil
	}
}

func pathOr(path string) string {
	if path == "" {
		return "the top level"
	}
	return strings.TrimPrefix(path, ".")
}
