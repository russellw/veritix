package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/russellw/veritix/internal/config"
	"github.com/russellw/veritix/internal/notify"
)

// sink is a webhook somebody pointed Veritix at.
type sink struct {
	*httptest.Server

	mu       sync.Mutex
	bodies   []string
	status   int
	attempts int
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, string(body))
		s.attempts++
		status := s.status
		s.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s
}

// await waits for a message, because delivery happens after the run's event
// stream has closed rather than before it.
func (s *sink) await(t *testing.T, within time.Duration) []notify.Message {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if m := s.messages(t); len(m) > 0 {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (s *sink) messages(t *testing.T) []notify.Message {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]notify.Message, 0, len(s.bodies))
	for _, b := range s.bodies {
		var m notify.Message
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			t.Fatalf("decode message: %v: %s", err, b)
		}
		out = append(out, m)
	}
	return out
}

func (s *sink) raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.bodies, "\n")
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

// notifying points a server at a sink, with the clock slowed right down so
// that tests drive it by hand.
func notifying(url string, tune ...func(*config.Notify)) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Schedule.Tick = time.Hour
		cfg.Notify.WebhookURL = url
		for _, f := range tune {
			f(&cfg.Notify)
		}
	}
}

// scheduledRun starts one window by hand and waits for the audit it began.
func (ts *testServer) scheduledRun(datasetID string) *runJSON {
	ts.t.Helper()

	ts.srv.sched.sweep(context.Background())
	sc, err := ts.st.Schedule(context.Background(), datasetID)
	if err != nil {
		ts.t.Fatalf("read schedule: %v", err)
	}
	if sc.LastRunID == "" {
		ts.t.Fatalf("the window started no audit: %q", sc.LastError)
	}
	return ts.awaitDone(sc.LastRunID)
}

// notifiable gives a dataset a due schedule that has asked to be told.
func (ts *testServer) notifiable(datasetID string) {
	ts.t.Helper()

	sc := ts.dueSchedule(datasetID, time.Hour)
	sc.Notify = true
	if err := ts.st.SetSchedule(context.Background(), sc); err != nil {
		ts.t.Fatalf("set schedule: %v", err)
	}
}

// The promise, read where it matters: at the bytes that left the process.
//
// It does not rest on this test's care either. Only a scheduled run notifies,
// and a scheduled run passes neither --include-values nor a model, so the
// document behind a message is a deterministic report with values off.
func TestNoNotificationCarriesCustomerData(t *testing.T) {
	sk := newSink(t)
	ts := newTestServerTuned(t, "", nil, notifying(sk.URL, func(n *config.Notify) {
		n.On = config.NotifyOnAny // every run, so there is the most to leak
	}))

	dir := copyFixture(t)
	ds := ts.registerCopy(dir)
	ts.notifiable(ds)

	// Two windows, so that the second message carries a comparison with
	// findings that moved — which is the part with the most text in it.
	ts.scheduledRun(ds)
	if len(sk.await(t, 10*time.Second)) == 0 {
		t.Fatal("the first scheduled audit told nobody")
	}

	orders := filepath.Join(dir, "orders.csv")
	body, err := os.ReadFile(orders)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orders,
		append(body, []byte("9999,CUS-999999,2024-03-01,10.00,GBP\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	ts.notifiable(ds)
	ts.scheduledRun(ds)

	deadline := time.Now().Add(10 * time.Second)
	for len(sk.messages(t)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	msgs := sk.messages(t)
	if len(msgs) < 2 {
		t.Fatalf("%d messages, want 2", len(msgs))
	}
	if msgs[1].Changes == nil {
		t.Fatal("the second message carried no comparison, so there is little in it to leak")
	}
	if len(msgs[1].Regressions) == 0 {
		t.Fatal("the second message named nothing that got worse")
	}

	// The same list the report tests and the agent's egress tests use: the
	// promise is one promise, held to in the same terms wherever data leaves.
	sent := sk.raw()
	for _, v := range rawValuesInFixtureForNotify {
		if strings.Contains(sent, v) {
			t.Errorf("a notification carried the raw value %q", v)
		}
	}
	// The offending rows are not in it either, nor the SQL that would find them.
	for _, s := range []string{"SELECT", "row_query", "count_query"} {
		if strings.Contains(sent, s) {
			t.Errorf("a notification carried %q", s)
		}
	}
}

// rawValuesInFixtureForNotify are verbatim contents of the dirty-retail files.
// It is deliberately the same list the report and agent egress tests use.
var rawValuesInFixtureForNotify = []string{
	"CUS-000001", "CUS-000005",
	"alice@example.com", "carol@example.com", "eve@example.com",
	"Alice Smith", "Frank Green", "Eve Black",
	"Zürich", "München", "Montréal",
	"Doohickey", "Widget",
}

func TestARegressionIsWhatMakesAMessage(t *testing.T) {
	sk := newSink(t)
	ts := newTestServerTuned(t, "", nil, notifying(sk.URL))

	dir := copyFixture(t)
	ds := ts.registerCopy(dir)
	ts.notifiable(ds)

	// A first audit of a dataset has nothing to compare against, so nothing
	// has got worse and nobody is told.
	ts.scheduledRun(ds)
	time.Sleep(200 * time.Millisecond)
	if n := sk.count(); n != 0 {
		t.Fatalf("the first audit sent %d messages, want none", n)
	}

	// A second audit of unchanged data has nothing to say either. This is the
	// whole reason the trigger is a regression and not a run: a nightly
	// "nothing changed" is how a channel gets muted.
	ts.notifiable(ds)
	ts.scheduledRun(ds)
	time.Sleep(200 * time.Millisecond)
	if n := sk.count(); n != 0 {
		t.Fatalf("an unchanged audit sent %d messages, want none", n)
	}

	// One more order referencing a customer who does not exist.
	orders := filepath.Join(dir, "orders.csv")
	body, err := os.ReadFile(orders)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orders,
		append(body, []byte("9999,CUS-999999,2024-03-01,10.00,GBP\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	ts.notifiable(ds)
	run := ts.scheduledRun(ds)

	msgs := sk.await(t, 10*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Event != notify.EventRegression {
		t.Errorf("event = %q, want %q", m.Event, notify.EventRegression)
	}
	if m.RunID != run.ID || m.DatasetID != ds {
		t.Errorf("message names run %q of dataset %q", m.RunID, m.DatasetID)
	}
	if m.Changes == nil || m.Changes.Worsened+m.Changes.New == 0 {
		t.Errorf("message carried no change: %+v", m.Changes)
	}
	var found bool
	for _, r := range m.Regressions {
		if r.Rule == "reference.orphan_values" && r.After > r.Before {
			found = true
		}
	}
	if !found {
		t.Errorf("the orphan references that got worse are not in the message: %+v", m.Regressions)
	}
}

// Somebody pressing Run is somebody watching, and does not need telling.
func TestAnAuditSomebodyStartedNotifiesNobody(t *testing.T) {
	sk := newSink(t)
	ts := newTestServerTuned(t, "", nil, notifying(sk.URL, func(n *config.Notify) {
		n.On = config.NotifyOnAny
	}))

	dir := copyFixture(t)
	ds := ts.registerCopy(dir)
	ts.notifiable(ds)

	ts.startRun(map[string]any{"dataset_id": ds})
	ts.startRun(map[string]any{"dataset_id": ds})

	time.Sleep(300 * time.Millisecond)
	if n := sk.count(); n != 0 {
		t.Errorf("audits somebody pressed sent %d messages", n)
	}
}

// A dataset that did not ask to be told is not told, even though the sink is
// configured and the clock is running.
func TestOnlyASchedulesOwnDatasetIsNotifiedAbout(t *testing.T) {
	sk := newSink(t)
	ts := newTestServerTuned(t, "", nil, notifying(sk.URL, func(n *config.Notify) {
		n.On = config.NotifyOnAny
	}))

	ds := ts.registerCopy(copyFixture(t))
	ts.dueSchedule(ds, time.Hour) // scheduled, but Notify is off

	ts.scheduledRun(ds)
	time.Sleep(300 * time.Millisecond)
	if n := sk.count(); n != 0 {
		t.Errorf("a schedule that did not ask sent %d messages", n)
	}
}

func TestSummaryDetailDropsTheTitlesAndLocations(t *testing.T) {
	sk := newSink(t)
	ts := newTestServerTuned(t, "", nil, notifying(sk.URL, func(n *config.Notify) {
		n.On = config.NotifyOnAny
		n.Detail = config.NotifyDetailSummary
	}))

	dir := copyFixture(t)
	ds := ts.registerCopy(dir)
	ts.notifiable(ds)
	ts.scheduledRun(ds)
	ts.notifiable(ds)

	orders := filepath.Join(dir, "orders.csv")
	body, err := os.ReadFile(orders)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orders,
		append(body, []byte("9999,CUS-999999,2024-03-01,10.00,GBP\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	ts.scheduledRun(ds)

	deadline := time.Now().Add(10 * time.Second)
	for len(sk.messages(t)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	msgs := sk.messages(t)
	if len(msgs) < 2 {
		t.Fatalf("%d messages, want 2", len(msgs))
	}
	if len(msgs[1].Regressions) != 0 {
		t.Errorf("summary detail still named %d findings", len(msgs[1].Regressions))
	}
	if msgs[1].Changes == nil {
		t.Error("summary detail dropped the counts too, which is all it was meant to keep")
	}
	// The dataset's name stays: a message that does not say which dataset is
	// not actionable at all.
	if msgs[1].Dataset == "" {
		t.Error("summary detail dropped the dataset's name")
	}
	// And no column or table name went with it.
	if raw := sk.raw(); strings.Contains(raw, "customer_id") || strings.Contains(raw, "orders_csv") {
		t.Errorf("summary detail carried a schema name: %s", raw)
	}
}

// A sink that is down does not cost anybody an audit.
func TestASinkThatWillNotAnswerDoesNotFailTheRun(t *testing.T) {
	sk := newSink(t)
	sk.mu.Lock()
	sk.status = http.StatusInternalServerError
	sk.mu.Unlock()

	ts := newTestServerTuned(t, "", nil, notifying(sk.URL, func(n *config.Notify) {
		n.On = config.NotifyOnAny
		n.Timeout = time.Second
	}))
	ds := ts.registerCopy(copyFixture(t))
	ts.notifiable(ds)

	run := ts.scheduledRun(ds)
	if run.Status != "succeeded" {
		t.Fatalf("the run failed because the webhook did: %s (%s)", run.Status, run.Message)
	}

	// It was retried rather than given up on at the first refusal.
	deadline := time.Now().Add(20 * time.Second)
	for sk.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := sk.count(); n != 3 {
		t.Errorf("the sink saw %d attempts, want 3", n)
	}

	// And the schedule's own record is about its window, not about a webhook.
	sc, err := ts.st.Schedule(context.Background(), ds)
	if err != nil {
		t.Fatalf("read schedule: %v", err)
	}
	if sc.LastError != "" {
		t.Errorf("a webhook failure was recorded against the window: %q", sc.LastError)
	}
}

func TestNoWebhookIsTheDefaultAndSendsNothing(t *testing.T) {
	ts := newTestServer(t, "")
	if ts.srv.notify != nil {
		t.Fatal("a sink exists with no webhook configured")
	}
	ds := ts.registerCopy(copyFixture(t))
	ts.notifiable(ds)
	// The point is that this does not panic on a nil sink.
	ts.scheduledRun(ds)
}

func TestAMessageIsJSONWithALinkWhenTheOperatorSaidWhereToLook(t *testing.T) {
	sk := newSink(t)
	ts := newTestServerTuned(t, "", nil, notifying(sk.URL, func(n *config.Notify) {
		n.On = config.NotifyOnAny
		n.BaseURL = "https://veritix.example.com/"
	}))
	ds := ts.registerCopy(copyFixture(t))
	ts.notifiable(ds)

	run := ts.scheduledRun(ds)
	msgs := sk.await(t, 10*time.Second)
	if len(msgs) != 1 {
		t.Fatalf("%d messages, want 1", len(msgs))
	}
	if want := "https://veritix.example.com/runs/" + run.ID; msgs[0].URL != want {
		t.Errorf("link = %q, want %q", msgs[0].URL, want)
	}
	if msgs[0].Findings == nil || msgs[0].Findings.Total == 0 {
		t.Errorf("message carried no finding counts: %+v", msgs[0].Findings)
	}
	if !bytes.Contains([]byte(sk.raw()), []byte(`"event":"audit"`)) {
		t.Errorf("message is not the shape a webhook consumer would expect: %s", sk.raw())
	}
}
