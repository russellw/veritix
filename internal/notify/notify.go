// Package notify tells somebody an audit found something, without telling them
// what is in the data.
//
// It exists because of the schedule. An audit somebody pressed is an audit
// somebody is watching; one that ran at two in the morning is not, and a
// comparison saying the export got worse is worth nothing sitting in a run
// history nobody opens until the next quarter.
//
// # What may be in a message
//
// The report's own comparison section, and nothing else: counts by status, and
// for each new or worsened finding its rule, severity, title, table and
// column. Never a cell value, never an offending row, never the SQL behind a
// finding.
//
// That titles and locations are admissible here — where internal/telemetry
// refuses even a table name — is a decision about audiences. A span is an
// access log leaving the machine for a collector; this goes to the people
// whose data it is, and "3 new errors" with no location is not something
// anybody can act on, which makes it one more message nobody reads.
// Notify.Detail is the switch for an operator posting into a channel wider
// than the data's audience.
//
// The promise that no cell value can reach a sink does not rest on this
// package's care. It rests on two decisions made earlier: only a scheduled run
// notifies, and a scheduled run neither passes --include-values nor runs a
// model. So the document a message is built from is a deterministic report
// with values off, which is the same document the four report writers are held
// to by TestDefaultReportContainsNoRawValues. TestNoNotificationCarriesCustomerData
// reads what actually left, in the same terms.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/report"
)

// Message is what leaves the process.
type Message struct {
	// Event is why this was sent: "regression", "failure" or "audit".
	Event string `json:"event"`
	// Dataset names the folder audited. It is the subject line — a message
	// that does not say which dataset is not actionable at all — and it
	// survives Detail: summary for the same reason.
	Dataset   string `json:"dataset"`
	DatasetID string `json:"dataset_id"`
	RunID     string `json:"run_id"`
	// Status is the run's terminal status: succeeded, failed or canceled.
	Status string `json:"status"`
	// Reason is why a run failed. It is Veritix's own diagnostic, which is the
	// class of text the log carries.
	Reason     string    `json:"reason,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
	// URL links to the run in the web interface, when the operator has said
	// where this instance is reachable.
	URL string `json:"url,omitempty"`
	// Version is the build that produced the run.
	Version string `json:"veritix_version,omitempty"`

	// Findings counts what the run found in total, which is the state.
	Findings *Counts `json:"findings,omitempty"`
	// Changes counts what moved since the previous audit, which is the
	// direction. A first audit of a dataset has none.
	Changes *report.DeltaSummary `json:"changes,omitempty"`
	// Regressions are the findings this run introduced or made worse, most
	// severe first. Absent under Detail: summary.
	Regressions []Regression `json:"regressions,omitempty"`
}

// Counts is a run's findings by severity.
type Counts struct {
	Total    int `json:"total"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Info     int `json:"info"`
}

// Regression is one finding that is new or worse than it was.
type Regression struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Status   string `json:"status"`
	Title    string `json:"title"`
	Table    string `json:"table,omitempty"`
	Column   string `json:"column,omitempty"`
	// Before and After are how many rows the finding affected. A new finding
	// has no before.
	Before int64 `json:"affected_count_before,omitempty"`
	After  int64 `json:"affected_count_after"`
}

// The values Message.Event takes.
const (
	EventRegression = "regression"
	EventFailure    = "failure"
	EventAudit      = "audit"
)

// Sink delivers messages. A zero URL means nobody is listening, which is the
// default and the shipped configuration.
type Sink struct {
	cfg  config.Notify
	log  *slog.Logger
	http *http.Client
}

// New builds a sink, or nil when no webhook is configured. A nil *Sink is safe
// to call Send on, so a caller does not have to check.
func New(cfg config.Notify, log *slog.Logger) *Sink {
	if cfg.WebhookURL == "" {
		return nil
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sink{cfg: cfg, log: log, http: &http.Client{Timeout: cfg.Timeout}}
}

// Wanted reports whether this run is worth a message under the configured
// trigger, and what kind it is.
//
// A failed run counts as a regression as well as a failure. A run that could
// not complete has no report, and treating its silence as "nothing got worse"
// is the mistake rule.never_applied exists to refuse: a nightly audit that has
// been failing for a month is exactly the thing nobody notices.
func (s *Sink) Wanted(status string, doc *report.Document, minSeverity string) (string, bool) {
	if s == nil {
		return "", false
	}
	switch status {
	case "canceled":
		// Somebody stopped it, so somebody already knows.
		return "", false
	case "failed":
		// Every trigger wants this one, including "regression".
		return EventFailure, true
	}

	regressions := 0
	if doc != nil && doc.Comparison != nil {
		regressions = doc.Comparison.Regressions(minSeverity)
	}
	switch {
	case regressions > 0 && s.cfg.On != config.NotifyOnFailure:
		return EventRegression, true
	case s.cfg.On == config.NotifyOnAny:
		return EventAudit, true
	default:
		return "", false
	}
}

// Detail reports whether messages carry the findings that moved.
func (s *Sink) Detail() string {
	if s == nil {
		return config.NotifyDetailSummary
	}
	return s.cfg.Detail
}

// MinSeverity is the threshold a regression has to reach.
func (s *Sink) MinSeverity() string {
	if s == nil {
		return "error"
	}
	return s.cfg.MinSeverity
}

// RunURL is where a person can read the run, or "" when the operator has not
// said where this instance is reachable.
func (s *Sink) RunURL(runID string) string {
	if s == nil || s.cfg.BaseURL == "" {
		return ""
	}
	return fmt.Sprintf("%s/runs/%s", trimSlash(s.cfg.BaseURL), runID)
}

func trimSlash(u string) string {
	for len(u) > 0 && u[len(u)-1] == '/' {
		u = u[:len(u)-1]
	}
	return u
}

// attempts is one delivery and two retries. A chat webhook that is briefly
// unreachable is the common failure and the one worth riding out; anything
// longer is a problem a person has to fix, and a message about last night's
// audit is stale by then anyway.
const attempts = 3

// backoff is the wait before each retry.
var backoff = []time.Duration{time.Second, 5 * time.Second}

// Send delivers a message, and reports why it could not.
//
// A delivery failure is never a run failure. The audit is done, its findings
// are recorded, and a report that nobody was told about is a great deal better
// than an audit that died because a chat server was down — which is the same
// rule a context server that will not answer gets.
func (s *Sink) Send(ctx context.Context, m Message) error {
	if s == nil {
		return nil
	}

	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("notify: encoding the message: %w", err)
	}

	var last error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}
		if last = s.post(ctx, body); last == nil {
			s.log.Info("sent a notification",
				"event", m.Event, "dataset", m.DatasetID, "run", m.RunID, "attempt", attempt+1)
			return nil
		}
		s.log.Warn("could not send a notification",
			"event", m.Event, "run", m.RunID, "attempt", attempt+1, "error", last)
	}
	return last
}

func (s *Sink) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("notify: building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("notify: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // the status is what matters

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The body is not read or logged: a sink's error page is somebody
		// else's content and there is nothing in it Veritix can act on.
		return fmt.Errorf("notify: the sink answered %s", resp.Status)
	}
	return nil
}
