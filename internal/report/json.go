// Package report renders an audit result for people and for machines.
package report

import (
	"encoding/json"
	"io"
	"time"

	"github.com/russellw/veritix/internal/agent"
	"github.com/russellw/veritix/internal/audit"
	"github.com/russellw/veritix/internal/finding"
	"github.com/russellw/veritix/internal/ingest"
	"github.com/russellw/veritix/internal/profile"
	"github.com/russellw/veritix/internal/rules"
)

// SchemaVersion identifies the JSON contract. It is declared in the output so
// that a consumer can tell when the shape has changed; internal types are
// deliberately not serialized directly, so refactoring them cannot silently
// break somebody's pipeline.
const SchemaVersion = "veritix.audit/v1"

// Options controls what a rendered report includes.
type Options struct {
	// IncludeValues permits verbatim cell values in the output: the most
	// frequent values, the lexicographic extremes, and the text of rows that
	// could not be parsed.
	//
	// Off by default. A report is a file that gets emailed, committed, and
	// pasted into tickets, and a report that quietly carries customer records
	// out of the building is a liability rather than a diagnostic.
	IncludeValues bool
	// Indent produces human-readable JSON.
	Indent bool
	// Baseline is a previous run of the same dataset to compare against. When
	// it is set the document carries a Comparison; when it is not, nothing
	// about the report changes.
	Baseline *Baseline
}

// Document is the root of the JSON report.
type Document struct {
	Schema  string      `json:"schema"`
	Version string      `json:"veritix_version,omitempty"`
	Run     RunInfo     `json:"run"`
	Dataset DatasetInfo `json:"dataset"`

	// Findings lead the document: they are the reason to read it.
	FindingSummary FindingSummary `json:"finding_summary"`
	Findings       []FindingInfo  `json:"findings"`

	// Comparison is what changed since a previous audit of the same dataset,
	// present only when a baseline was given. It is here rather than in a
	// document of its own because "is this getting better or worse" is the
	// question somebody asks on every audit after the first, and it has to be
	// answered beside the findings it is about.
	Comparison *Delta `json:"comparison,omitempty"`

	Tables   []TableInfo  `json:"tables"`
	Skipped  []SkipInfo   `json:"skipped_files,omitempty"`
	Warnings []NoteInfo   `json:"warnings,omitempty"`
	Redacted RedactedInfo `json:"redaction"`

	// Agent describes the model-driven investigation, when one ran. It is
	// absent from a deterministic-only audit rather than present and empty, so
	// that a reader can tell at a glance whether a model was involved at all.
	Agent *AgentInfo `json:"agent,omitempty"`

	// Proposals are rules suggested for future audits. They are not findings
	// and nothing has been applied: they are here so that a reader of the
	// report knows what was suggested, and so that whoever accepts one is
	// looking at the same document as everybody else.
	Proposals []ProposalInfo `json:"rule_proposals,omitempty"`
}

// ProposalInfo is one proposed rule, as a report describes it.
//
// It carries the shape of the rule and not its contents. A one_of rule's
// permitted set is materialized from the data — see rules.Materialize — so it
// is cell values, and a report is a file that gets emailed, committed and
// pasted into tickets. The count is here; the values are written out by
// rules.RenderProposals, which produces a rules file rather than a report, and
// by --include-values for a reader who has already decided that this report may
// carry them.
type ProposalInfo struct {
	// ID is stable across runs: it identifies what the rule asserts, so the
	// same expectation proposed on three runs is one thing to decide about.
	ID   string `json:"id"`
	Rule string `json:"rule"`

	Description string `json:"description,omitempty"`
	Rationale   string `json:"rationale,omitempty"`

	// Target is the source name a person recognizes; Table is the SQL name the
	// rule is written against.
	Target string `json:"target"`
	Table  string `json:"table"`
	Column string `json:"column,omitempty"`

	Expect   string `json:"expect"`
	Severity string `json:"severity"`

	// ViolationsNow is what the rule measured when it was proposed. Zero is
	// the good case: an expectation that already holds.
	ViolationsNow int64 `json:"violations_now"`

	// PermittedValueCount is how many values a one_of rule would allow.
	PermittedValueCount int `json:"permitted_value_count,omitempty"`
	// PermittedValues are those values, present only when the report was asked
	// to include verbatim values.
	PermittedValues []string `json:"permitted_values,omitempty"`
}

// AgentInfo is what the agentic pass did.
//
// It is in the report rather than only in the trace because a reader needs to
// know that a model was involved, which one, whether it was permitted to see
// cell values, and whether it finished — before they weigh anything it found.
// The full trace is a separate document served by the API; this is the part
// that belongs beside the findings.
type AgentInfo struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`

	Steps     int `json:"steps"`
	ToolCalls int `json:"tool_calls"`
	// Findings is how many the agent contributed, and NotReproduced how many
	// it proposed that the engine measured at zero and refused to record.
	Findings      int `json:"findings"`
	NotReproduced int `json:"not_reproduced"`

	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	// ValuesSent records whether this run was permitted to send cell values to
	// the model. It is the single most important fact about an agentic run.
	ValuesSent bool `json:"values_sent_to_model"`
	// ValuesWithheld is how many were replaced by their shape.
	ValuesWithheld int `json:"values_withheld"`
	// ContextDocuments is how many of the customer's own documents the model
	// read during this run, and ContextServers what they were named after.
	//
	// A report is a file that gets emailed, so this is a count and the servers'
	// names and nothing else — the documents' contents were sent to a model and
	// belong in the trace, where somebody who wants them can read every byte.
	// It is here at all because a finding that exists because a data dictionary
	// said so is a finding whose reader deserves to know a document was
	// consulted.
	ContextDocuments int      `json:"context_documents,omitempty"`
	ContextServers   []string `json:"context_servers,omitempty"`

	// Stopped says why the investigation ended, and Complete whether that was
	// because it finished rather than because it ran out of budget. A report
	// should not imply a thorough investigation when the agent was cut off.
	Stopped  string `json:"stopped"`
	Complete bool   `json:"complete"`

	DurationMS int64 `json:"duration_ms"`
}

// RunInfo describes the audit itself.
type RunInfo struct {
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
}

// DatasetInfo describes what was audited.
type DatasetInfo struct {
	Root           string `json:"root"`
	FileCount      int    `json:"file_count"`
	TableCount     int    `json:"table_count"`
	ColumnCount    int    `json:"column_count"`
	RowCount       int64  `json:"row_count"`
	UnreadableRows int64  `json:"unreadable_rows"`
}

// RedactedInfo records what was withheld, so that a reader can tell the
// difference between "this column has no notable values" and "values were not
// included in this report".
type RedactedInfo struct {
	ValuesIncluded bool   `json:"values_included"`
	Note           string `json:"note,omitempty"`
}

// TableInfo is one table's profile.
type TableInfo struct {
	Name     string       `json:"name"`
	Source   string       `json:"source"`
	File     string       `json:"file"`
	Sheet    string       `json:"sheet,omitempty"`
	RowCount int64        `json:"row_count"`
	Columns  []ColumnInfo `json:"columns"`

	Reading  *ReadingInfo `json:"reading,omitempty"`
	Rejected *RejectInfo  `json:"rejected_rows,omitempty"`
	Notes    []NoteInfo   `json:"notes,omitempty"`
}

// ReadingInfo records how a file was parsed. It is reported because a
// misdetected dialect or encoding invalidates everything downstream, and a
// reader needs to be able to check the assumption rather than trust it.
type ReadingInfo struct {
	Format    string `json:"format"`
	Delimiter string `json:"delimiter,omitempty"`
	Quote     string `json:"quote,omitempty"`
	Encoding  string `json:"encoding,omitempty"`
	HasHeader bool   `json:"has_header"`
	SkipRows  int    `json:"skip_rows,omitempty"`
	HeaderRow int    `json:"header_row,omitempty"`
}

// RejectInfo describes rows that could not be read at all.
type RejectInfo struct {
	Count   int64             `json:"count"`
	Samples []RejectionSample `json:"samples,omitempty"`
}

// RejectionSample is one unreadable row.
type RejectionSample struct {
	Line    int64  `json:"line"`
	Column  string `json:"column,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
	// RawLine is verbatim file content and appears only when values are
	// explicitly included.
	RawLine string `json:"raw_line,omitempty"`
}

// ColumnInfo is one column's profile.
type ColumnInfo struct {
	Name     string `json:"name"`
	Original string `json:"original_name,omitempty"`
	Position int    `json:"position"`

	InferredType  string  `json:"inferred_type"`
	DeclaredType  string  `json:"declared_type,omitempty"`
	Conformance   float64 `json:"conformance"`
	Nonconforming int64   `json:"nonconforming_values"`
	// ClosestType and ClosestMatch describe the nearest type match for a
	// column that was classified as text.
	ClosestType  string  `json:"closest_type,omitempty"`
	ClosestMatch float64 `json:"closest_match,omitempty"`

	Rows               int64 `json:"rows"`
	Nulls              int64 `json:"nulls"`
	Blanks             int64 `json:"blanks"`
	Missing            int64 `json:"missing_total"`
	Distinct           int64 `json:"distinct"`
	DistinctNormalized int64 `json:"distinct_normalized"`
	Unique             bool  `json:"unique"`

	MinLength int64   `json:"min_length"`
	MaxLength int64   `json:"max_length"`
	AvgLength float64 `json:"avg_length"`

	LeadingWhitespace  int64 `json:"leading_whitespace"`
	TrailingWhitespace int64 `json:"trailing_whitespace"`

	Sentinels []ValueInfo `json:"sentinels,omitempty"`
	Shapes    []ValueInfo `json:"shapes,omitempty"`

	Numeric  *NumericInfo  `json:"numeric,omitempty"`
	Temporal *TemporalInfo `json:"temporal,omitempty"`

	// TopValues, MinValue, and MaxValue are verbatim cell contents and appear
	// only when values are explicitly included.
	TopValues []ValueInfo `json:"top_values,omitempty"`
	MinValue  string      `json:"min_value,omitempty"`
	MaxValue  string      `json:"max_value,omitempty"`
}

// ValueInfo is a value or shape with its frequency.
type ValueInfo struct {
	Value string  `json:"value"`
	Count int64   `json:"count"`
	Share float64 `json:"share"`
}

// NumericInfo summarizes numeric content.
type NumericInfo struct {
	Count    int64   `json:"count"`
	Min      float64 `json:"min"`
	Max      float64 `json:"max"`
	Mean     float64 `json:"mean"`
	StdDev   float64 `json:"stddev"`
	P25      float64 `json:"p25"`
	Median   float64 `json:"median"`
	P75      float64 `json:"p75"`
	Negative int64   `json:"negative"`
	Zero     int64   `json:"zero"`
	Outliers int64   `json:"outliers"`
}

// TemporalInfo summarizes date content.
type TemporalInfo struct {
	Count       int64        `json:"count"`
	Earliest    string       `json:"earliest,omitempty"`
	Latest      string       `json:"latest,omitempty"`
	Formats     []FormatInfo `json:"formats,omitempty"`
	Ambiguous   int64        `json:"ambiguous"`
	Future      int64        `json:"future"`
	Implausible int64        `json:"implausible"`
}

// FormatInfo is a date format and how many values use it.
type FormatInfo struct {
	Pattern     string `json:"pattern"`
	Description string `json:"description"`
	Count       int64  `json:"count"`
}

// NoteInfo is an observation about a file or table.
type NoteInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SkipInfo is a file that was not read.
type SkipInfo struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

// WriteJSON renders a run as JSON.
func WriteJSON(w io.Writer, res *audit.Result, version string, opts Options) error {
	return RenderJSON(w, Build(res, version, opts), opts)
}

// RenderJSON encodes an already-built document.
//
// Every format has one of these beside its Write function, because a caller
// that needs the document for something else — the CLI's --baseline
// comparison, the server's stored blob — must not build it twice. Two
// documents from one run is two chances for a report and the thing that
// gates on it to disagree.
func RenderJSON(w io.Writer, doc *Document, opts Options) error {
	enc := json.NewEncoder(w)
	if opts.Indent {
		enc.SetIndent("", "  ")
	}
	return enc.Encode(doc)
}

// buildAgent summarizes the agent's trace for the report.
func buildAgent(t *agent.Trace) *AgentInfo {
	if t == nil {
		return nil
	}
	var calls int
	for _, s := range t.Steps {
		calls += len(s.Calls)
	}
	return &AgentInfo{
		Provider:       t.Provider,
		Model:          t.Model,
		Steps:          len(t.Steps),
		ToolCalls:      calls,
		Findings:       t.Findings,
		NotReproduced:  t.Refused,
		InputTokens:    t.Usage.Input + t.Usage.CacheRead + t.Usage.CacheWrite,
		OutputTokens:   t.Usage.Output,
		ValuesSent:     t.ValuesAllowed,
		ValuesWithheld: t.Redaction.Shaped,
		ContextDocuments: func() int {
			if t.Context == nil {
				return 0
			}
			return t.Context.Read
		}(),
		ContextServers: contextServerNames(t),
		Stopped:        string(t.Stopped),
		Complete:       t.Stopped.Complete(),
		DurationMS:     t.Duration.Milliseconds(),
	}
}

// Build converts a run into the report document.
func Build(res *audit.Result, version string, opts Options) *Document {
	s := res.Summarize()

	doc := &Document{
		Schema:  SchemaVersion,
		Version: version,
		Run: RunInfo{
			StartedAt:  res.StartedAt,
			DurationMS: res.Duration.Milliseconds(),
		},
		Dataset: DatasetInfo{
			Root:           s.Root,
			FileCount:      s.Files,
			TableCount:     s.Tables,
			ColumnCount:    s.Columns,
			RowCount:       s.Rows,
			UnreadableRows: s.Rejected,
		},
		Redacted: RedactedInfo{ValuesIncluded: opts.IncludeValues},
	}

	doc.Agent = buildAgent(res.Trace)
	doc.Proposals = buildProposals(res.Proposals, opts)
	doc.Findings, doc.FindingSummary = buildFindings(res)
	if !opts.IncludeValues {
		doc.Redacted.Note = "Verbatim cell values are omitted. Counts, distributions, " +
			"and value shapes are complete. Re-run with values included to see examples."
	}

	for _, sk := range res.Dataset.Skipped {
		doc.Skipped = append(doc.Skipped, SkipInfo{File: sk.Rel, Reason: sk.Reason})
	}

	byName := make(map[string]*profile.Table, len(res.Profile.Tables))
	for _, pt := range res.Profile.Tables {
		byName[pt.Name] = pt
	}

	for _, lt := range res.Loaded.Tables {
		pt := byName[lt.Ref.Name]
		if pt == nil {
			continue
		}
		doc.Tables = append(doc.Tables, buildTable(lt, pt, opts))
	}

	// The comparison goes last because it reads the document that has just
	// been built: findings, tables and all.
	if opts.Baseline != nil && opts.Baseline.Document != nil {
		doc.Comparison = Compare(opts.Baseline.Document, doc)
		doc.Comparison.Baseline.RunID = opts.Baseline.RunID
		doc.Comparison.Baseline.Source = opts.Baseline.Source
	}
	return doc
}

// buildProposals describes what was proposed without reproducing what it
// permits.
func buildProposals(ps []rules.Proposal, opts Options) []ProposalInfo {
	out := make([]ProposalInfo, 0, len(ps))
	for _, p := range ps {
		out = append(out, DescribeProposal(p, opts.IncludeValues))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DescribeProposal renders one proposal the way a report describes it.
//
// It is exported because the API lists proposals too, and a second renderer
// would eventually disagree with this one about what a reader is shown — the
// same argument that keeps the API serving the stored report document verbatim
// rather than rebuilding it.
func DescribeProposal(p rules.Proposal, includeValues bool) ProposalInfo {
	severity := finding.Error
	if p.Rule.Severity != nil {
		severity = *p.Rule.Severity
	}
	target := p.Display
	if target == "" {
		target = p.Rule.Table
	}
	if p.Rule.Column != "" {
		target += "." + p.Rule.Column
	}

	info := ProposalInfo{
		ID:                  p.ID(),
		Rule:                p.Rule.ID,
		Description:         p.Rule.Description,
		Rationale:           p.Rationale,
		Target:              target,
		Table:               p.Rule.Table,
		Column:              p.Rule.Column,
		Expect:              string(p.Rule.Expect),
		Severity:            severity.String(),
		ViolationsNow:       p.ViolationsNow,
		PermittedValueCount: len(p.Rule.Values),
	}
	if includeValues {
		info.PermittedValues = p.Rule.Values
	}
	return info
}

func buildTable(lt *ingest.Table, pt *profile.Table, opts Options) TableInfo {
	ti := TableInfo{
		Name:     pt.Name,
		Source:   pt.Display,
		File:     lt.Ref.File.Rel,
		Sheet:    lt.Ref.Sheet,
		RowCount: pt.RowCount,
		Reading:  buildReading(lt),
	}

	for _, n := range lt.Notes {
		ti.Notes = append(ti.Notes, NoteInfo{Code: n.Code, Message: n.Message})
	}

	if lt.RejectCount > 0 {
		ri := &RejectInfo{Count: lt.RejectCount}
		for _, r := range lt.Rejects {
			s := RejectionSample{
				Line:    r.Line,
				Column:  r.Column,
				Reason:  r.ErrorType,
				Message: r.Message,
			}
			if opts.IncludeValues {
				s.RawLine = r.RawLine
			}
			ri.Samples = append(ri.Samples, s)
		}
		ti.Rejected = ri
	}

	for _, c := range pt.Columns {
		ti.Columns = append(ti.Columns, buildColumn(c, opts))
	}
	return ti
}

func buildReading(lt *ingest.Table) *ReadingInfo {
	switch {
	case lt.Dialect != nil:
		return &ReadingInfo{
			Format:    "delimited",
			Delimiter: lt.Dialect.Delimiter,
			Quote:     lt.Dialect.Quote,
			Encoding:  string(lt.Dialect.Encoding),
			HasHeader: lt.Dialect.HasHeader,
			SkipRows:  lt.Dialect.SkipRows,
		}
	case lt.Sheet != nil:
		return &ReadingInfo{
			Format:    "excel",
			HasHeader: true,
			HeaderRow: lt.Sheet.HeaderRow,
		}
	default:
		return nil
	}
}

func buildColumn(c *profile.Column, opts Options) ColumnInfo {
	ci := ColumnInfo{
		Name:               c.Name,
		Position:           c.Ordinal,
		InferredType:       string(c.Inferred.Kind),
		DeclaredType:       c.DeclaredType,
		Conformance:        round(c.Inferred.Conformance),
		Nonconforming:      c.Inferred.Nonconforming,
		Rows:               c.Total,
		Nulls:              c.Nulls,
		Blanks:             c.Blanks,
		Missing:            c.Missing(),
		Distinct:           c.Distinct,
		DistinctNormalized: c.DistinctNormalized,
		Unique:             c.Unique(),
		MinLength:          c.MinLength,
		MaxLength:          c.MaxLength,
		AvgLength:          round(c.AvgLength),
		LeadingWhitespace:  c.LeadingWhitespace,
		TrailingWhitespace: c.TrailingWhitespace,
	}
	if c.Original != c.Name {
		ci.Original = c.Original
	}
	if c.Inferred.Kind == profile.KindText && c.Inferred.BestCandidate != "" {
		ci.ClosestType = string(c.Inferred.BestCandidate)
		ci.ClosestMatch = round(c.Inferred.BestConformance)
	}

	ci.Sentinels = values(c.Sentinels)
	ci.Shapes = values(c.Shapes)

	if c.Numeric != nil {
		n := c.Numeric
		ci.Numeric = &NumericInfo{
			Count: n.Count, Min: round(n.Min), Max: round(n.Max),
			Mean: round(n.Mean), StdDev: round(n.StdDev),
			P25: round(n.P25), Median: round(n.Median), P75: round(n.P75),
			Negative: n.Negative, Zero: n.Zero, Outliers: n.Outliers,
		}
	}
	if c.Temporal != nil {
		tp := c.Temporal
		ti := &TemporalInfo{
			Count: tp.Count, Earliest: tp.Min, Latest: tp.Max,
			Ambiguous: tp.Ambiguous, Future: tp.Future, Implausible: tp.Implausible,
		}
		for _, f := range tp.Formats {
			ti.Formats = append(ti.Formats, FormatInfo{
				Pattern: f.Format, Description: f.Example, Count: f.Count,
			})
		}
		ci.Temporal = ti
	}

	if opts.IncludeValues {
		ci.TopValues = values(c.TopValues)
		ci.MinValue = c.MinValue
		ci.MaxValue = c.MaxValue
	}
	return ci
}

func values(in []profile.ValueCount) []ValueInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]ValueInfo, len(in))
	for i, v := range in {
		out[i] = ValueInfo{Value: v.Value, Count: v.Count, Share: round(v.Share)}
	}
	return out
}

// round trims floating-point noise so that two runs over identical data
// produce byte-identical reports.
func round(f float64) float64 {
	const places = 1e6
	return float64(int64(f*places+copysign(0.5, f))) / places
}

func copysign(magnitude, sign float64) float64 {
	if sign < 0 {
		return -magnitude
	}
	return magnitude
}

// contextServerNames lists the servers a run could read documents from.
//
// Only the ones that answered: a server that was configured and unreachable is
// a fact about the deployment rather than about this dataset, and it is in the
// trace with the reason it failed.
func contextServerNames(t *agent.Trace) []string {
	if t.Context == nil {
		return nil
	}
	var out []string
	for _, s := range t.Context.Servers {
		if s.Error == "" {
			out = append(out, s.Name)
		}
	}
	return out
}
