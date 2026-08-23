// Package config holds Veritix's runtime configuration and the rules for
// assembling it from defaults, a YAML file, and the environment.
//
// Precedence, lowest to highest: defaults, config file, environment, flags.
// Flags are applied by the CLI layer after Load returns.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/russellw/veritix/internal/buildinfo"
)

// EnvPrefix is prepended to every environment variable Veritix reads.
const EnvPrefix = "VERITIX_"

// Config is the complete runtime configuration.
type Config struct {
	Log    Log    `yaml:"log"`
	Server Server `yaml:"server"`
	Engine Engine `yaml:"engine"`
	LLM    LLM    `yaml:"llm"`
	// Context is where the customer's own documents come from. Empty by
	// default: a default install talks to nobody, which is the same promise
	// llm.provider: none makes.
	Context Context `yaml:"context"`
	OTel    OTel    `yaml:"otel"`
	// Schedule is the clock that starts the audits nobody pressed.
	Schedule Schedule `yaml:"schedule"`
	// Notify is how somebody finds out about those audits.
	Notify Notify `yaml:"notify"`
}

// Notify configures where Veritix tells somebody that a scheduled audit found
// something.
//
// Off by default, for the reason llm.provider and otel.enabled are: a webhook
// is a network egress from a process holding data the customer declined to
// send to a vendor. Setting a URL here is Veritix's switch; which datasets use
// it is a flag on each schedule, so that turning it on does not sign every
// dataset up at once.
//
// What may be in a message is the decision this section rests on. A span
// carries counts and never names, because a collector is an access log leaving
// the machine; a notification is not that. It goes to the audit's own
// audience, and "3 new errors" with no location is not something anybody can
// act on, which makes it another thing nobody reads. So a message carries the
// same titles, rules and locations the report's own comparison section
// carries — and never a cell value, never an offending row. See
// internal/notify for what that rests on.
type Notify struct {
	// WebhookURL receives a JSON POST. Empty is off, and is the default.
	//
	// Webhook and not email, deliberately: Teams, Slack, PagerDuty and a
	// two-line script all take one, where email means credentials, TLS modes,
	// sender identity, recipient lists, retries and bounces, to reimplement a
	// bridge every customer already has.
	WebhookURL string `yaml:"webhook_url"`
	// On is what makes a message: regression, failure, or any.
	On string `yaml:"on"`
	// MinSeverity is how bad a new or worsened finding has to be to count as a
	// regression. It is the same threshold --fail-on-regression uses.
	MinSeverity string `yaml:"min_severity"`
	// Detail is findings or summary. Summary drops the titles and locations,
	// for an operator posting into a channel wider than the data's audience.
	Detail string `yaml:"detail"`
	// BaseURL is where this instance is reachable, e.g.
	// https://veritix.example.com, so that a message can link to the run.
	// Empty leaves the link out rather than guessing from the listen address,
	// which behind any proxy would be wrong.
	BaseURL string `yaml:"base_url"`
	// Timeout bounds one delivery attempt.
	Timeout time.Duration `yaml:"timeout"`
}

// The values Notify.On takes.
const (
	// NotifyOnRegression is a run that found new or worsened findings at or
	// above the severity threshold, and also a run that could not complete: a
	// run with no report cannot be shown not to have regressed, which is
	// rule.never_applied's argument about a check that never ran.
	NotifyOnRegression = "regression"
	// NotifyOnFailure is only a run that could not complete.
	NotifyOnFailure = "failure"
	// NotifyOnAny is every scheduled run, including a clean one.
	NotifyOnAny = "any"
)

// The values Notify.Detail takes.
const (
	NotifyDetailFindings = "findings"
	NotifyDetailSummary  = "summary"
)

// Schedule configures the clock, not the schedules themselves.
//
// A dataset is audited on a clock only if somebody asked for it, and that
// request is a row in the run store rather than a setting here: schedules are
// per dataset, and datasets are registered at runtime with ids no config file
// can name. What lives here is whether this process runs the clock at all,
// which matters because a data directory may be shared — a veritix mcp beside
// a veritix serve must not both be firing the same windows, and only serve
// does.
type Schedule struct {
	// Enabled runs the clock. Turning it off leaves every schedule stored and
	// fires none of them.
	Enabled bool `yaml:"enabled"`
	// Tick is how often due schedules are looked for. It is not how precisely
	// an audit starts on time: at most one audit is started per tick, which is
	// what staggers a restart where several datasets are due at once.
	Tick time.Duration `yaml:"tick"`
}

// Context configures the MCP servers Veritix reads the customer's own
// documents from — data dictionaries, warehouse catalogs, tickets.
//
// It has no environment-variable form, and that is deliberate rather than an
// omission: a list of subprocesses with their arguments does not survive being
// flattened into VERITIX_CONTEXT_SERVERS, and this is the one setting that
// says which programs Veritix will start. It belongs in a file somebody wrote
// and can read back. In a container that file arrives on a mounted volume and
// VERITIX_CONFIG names it, which is the shape deploy/ already uses.
//
// Whatever these servers return is sent to the model verbatim: a data
// dictionary rendered as shapes would explain nothing, so configuring one is
// the same class of decision as llm.allow_sample_values. Every request made
// and every document admitted is in the run's trace. See internal/mcpclient.
type Context struct {
	// Servers are the MCP servers to connect to, in the order they are listed.
	Servers []ContextServer `yaml:"servers"`
	// MaxDocumentBytes truncates a single document before it reaches the
	// model. Zero picks a default.
	MaxDocumentBytes int `yaml:"max_document_bytes"`
	// ConnectTimeout bounds starting one server and listing what it has. Zero
	// picks a default.
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
}

// ContextServer is one such server, started as a subprocess and spoken to over
// stdio.
type ContextServer struct {
	// Name is Veritix's handle for it, shown against every document it
	// contributes.
	Name string `yaml:"name"`
	// Command is the executable, and Args its arguments.
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Env adds NAME=VALUE entries to the command's environment, which is where
	// a credential for the customer's own ticket system goes.
	Env []string `yaml:"env"`
}

// Log controls diagnostic output.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is text (human-readable) or json (machine-readable).
	Format string `yaml:"format"`
}

// OTel configures OpenTelemetry export.
//
// Off by default, for the reason llm.provider is "none" by default: an OTLP
// endpoint is a network egress, and this process holds data the customer
// declined to send to a vendor. A build that started exporting because an
// ambient OTEL_EXPORTER_OTLP_ENDPOINT happened to be set would be Veritix
// making that decision on their behalf.
//
// Once it is on, the standard OTEL_EXPORTER_OTLP_* variables are honored by
// the exporters themselves, so an operator's existing collector configuration
// works. Enabling is Veritix's switch; where to send is theirs.
type OTel struct {
	// Enabled turns the exporters on. Nothing is exported without it.
	Enabled bool `yaml:"enabled"`
	// Endpoint is the collector's OTLP/HTTP base URL, e.g.
	// http://localhost:4318. Empty defers to OTEL_EXPORTER_OTLP_ENDPOINT.
	Endpoint string `yaml:"endpoint"`
	// ServiceName names this instance in the collector.
	ServiceName string `yaml:"service_name"`
	// SampleRatio is the fraction of traces recorded, 0 to 1. An audit is a
	// minutes-long operation somebody asked for, so the useful default is all
	// of them: sampling is for request floods and there is no flood here.
	SampleRatio float64 `yaml:"sample_ratio"`
	// ExportTimeout bounds one export attempt and the flush at shutdown, so
	// that a collector that has gone away cannot turn a clean exit into a
	// hang.
	ExportTimeout time.Duration `yaml:"export_timeout"`
}

// Server configures the HTTP interface.
type Server struct {
	// Addr is the listen address. It defaults to loopback: exposing an
	// instance to a network is a deliberate act, not an accident.
	Addr string `yaml:"addr"`
	// AuthToken, when set, is required as a bearer token on every API
	// request. Binding to a non-loopback address without one is refused.
	AuthToken string `yaml:"auth_token"`
	// DataDir holds the run store, uploaded datasets, and cached reports.
	DataDir string `yaml:"data_dir"`
	// MaxUploadBytes caps one dataset upload. Uploading is how a business
	// user supplies data, so this has to be generous enough for a real export
	// and small enough that one request cannot fill the disk.
	MaxUploadBytes int64 `yaml:"max_upload_bytes"`
	// SourceURL is where this build's source can be obtained, offered by
	// /health and shown in the web interface's footer. It defaults to the
	// upstream repository, which is the right answer for an unmodified build
	// and the wrong one for a modified build served to other people: AGPL
	// section 13 obliges whoever modified it to offer *their* source. Point
	// this at their repository and the interface makes that offer for them.
	SourceURL string `yaml:"source_url"`

	// RetainDatabases is how long a finished run's ingested data is kept
	// before it is discarded. Zero keeps every one of them forever.
	//
	// Every run leaves a DuckDB file behind so that a finding's offending rows
	// can be fetched afterwards, at roughly a third of the dataset's size.
	// Auditing by hand never made that a problem; a nightly audit of a two
	// gigabyte export is about 700 MB a night, which fills a disk in a
	// quarter. What is discarded is the ingested copy of the customer's data
	// and nothing else: the run, its report, its findings and its trace are
	// the audit trail, they are small, and they stay. The most recent run of
	// each dataset that still has one keeps it whatever this says, so the
	// newest audit's rows are always there to look at.
	RetainDatabases time.Duration `yaml:"retain_databases"`

	ReadHeaderTimeout time.Duration `yaml:"read_header_timeout"`
	ShutdownTimeout   time.Duration `yaml:"shutdown_timeout"`
}

// Engine configures the DuckDB analysis engine and the limits placed on it.
// The limits exist so that one pathological dataset or one runaway
// agent-authored query cannot exhaust the host.
type Engine struct {
	// MemoryLimit is passed to DuckDB, e.g. "4GB". Empty means DuckDB's own
	// default (about 80% of system RAM).
	MemoryLimit string `yaml:"memory_limit"`
	// Threads caps DuckDB's worker threads. Zero means one per core.
	Threads int `yaml:"threads"`
	// QueryTimeout bounds any single query, which for the deterministic
	// pipeline means one measurement over a whole column.
	//
	// It has to be sized for the dataset the product exists to audit rather
	// than for the fixtures: profiling one column of a twenty-million-row
	// table takes minutes, and a limit below that does not fail the audit —
	// it drops the column, leaves a stub that reads as clean, and reports the
	// rest as if the table had been looked at. See column.not_profiled, which
	// is the finding that makes that visible, and AgentQueryTimeout, which is
	// the shorter bound the reason for a short one actually belongs to.
	QueryTimeout time.Duration `yaml:"query_timeout"`
	// AgentQueryTimeout bounds a single statement the model wrote.
	//
	// This is where "one runaway query must not exhaust the host" belongs. A
	// measurement Veritix wrote costs what the data costs; a model's SQL is
	// unreviewed, arrives up to forty times a run, and is the only SQL here
	// nobody chose. Zero falls back to QueryTimeout.
	AgentQueryTimeout time.Duration `yaml:"agent_query_timeout"`
	// MaxResultRows caps rows returned to a caller from an ad-hoc query.
	MaxResultRows int `yaml:"max_result_rows"`
	// TempDir is where DuckDB spills to disk. Empty means the system temp dir.
	TempDir string `yaml:"temp_dir"`
}

// LLM configures the model provider used by the agentic auditor.
type LLM struct {
	// Provider is one of: none, anthropic, openai-compatible.
	Provider string `yaml:"provider"`
	// Model is the provider-specific model identifier.
	Model string `yaml:"model"`
	// BaseURL overrides the provider endpoint. This is how a customer points
	// Veritix at Ollama, vLLM, LM Studio, or any other local server.
	BaseURL string `yaml:"base_url"`
	// APIKey is read from config or the environment. It is never logged.
	APIKey string `yaml:"api_key"`

	// AllowSampleValues lifts the default egress policy and permits raw cell
	// values (after redaction) to be sent to the model. Off by default: the
	// product's premise is that customer data stays on customer hardware.
	AllowSampleValues bool `yaml:"allow_sample_values"`

	// Effort asks the model for more or less deliberation, passed through to
	// whatever the provider calls it: Anthropic's output effort, and
	// reasoning_effort for the OpenAI dialect. Empty sends nothing, which
	// leaves the model's own default in place.
	//
	// The vocabulary is deliberately not enumerated here. Providers disagree
	// about it — "none", "minimal", "low", "medium", "high" — a value one
	// accepts is a 400 from another, and a list Veritix maintained would be
	// wrong within a release. The provider's error is the better teacher.
	//
	// "none" is what makes a hybrid reasoning model usable on a CPU. Ollama
	// honors it in the OpenAI dialect, and qwen3.5-35b-a3b answers a tool call
	// in 14 tokens with it against 73 without — five times the generation, on
	// hardware where generation is the whole cost, for reasoning the dialect
	// then throws away.
	Effort string `yaml:"effort"`

	// MaxSteps bounds the agent's tool-calling loop.
	MaxSteps int `yaml:"max_steps"`
	// TokenBudget bounds total tokens across one audit run. Zero means no cap.
	TokenBudget int `yaml:"token_budget"`
	// RequestTimeout bounds a single model call.
	RequestTimeout time.Duration `yaml:"request_timeout"`
}

// Provider identifiers.
const (
	ProviderNone             = "none"
	ProviderAnthropic        = "anthropic"
	ProviderOpenAICompatible = "openai-compatible"
)

// Default returns the configuration used when nothing else is specified.
func Default() Config {
	return Config{
		Log: Log{
			Level:  "info",
			Format: "text",
		},
		Server: Server{
			Addr:              "127.0.0.1:8080",
			DataDir:           defaultDataDir(),
			MaxUploadBytes:    2 << 30, // 2 GiB
			SourceURL:         buildinfo.SourceURL,
			RetainDatabases:   14 * 24 * time.Hour,
			ReadHeaderTimeout: 10 * time.Second,
			ShutdownTimeout:   15 * time.Second,
		},
		Engine: Engine{
			QueryTimeout:      30 * time.Minute,
			AgentQueryTimeout: 2 * time.Minute,
			MaxResultRows:     10_000,
		},
		LLM: LLM{
			Provider:       ProviderNone,
			MaxSteps:       40,
			RequestTimeout: 10 * time.Minute,
		},
		Schedule: Schedule{
			Enabled: true,
			Tick:    30 * time.Second,
		},
		Notify: Notify{
			On:          NotifyOnRegression,
			MinSeverity: "error",
			Detail:      NotifyDetailFindings,
			Timeout:     10 * time.Second,
		},
		OTel: OTel{
			Enabled:       false,
			ServiceName:   "veritix",
			SampleRatio:   1,
			ExportTimeout: 10 * time.Second,
		},
	}
}

func defaultDataDir() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "veritix")
	}
	return ".veritix"
}

// Load assembles configuration from defaults, an optional YAML file, and the
// environment. An empty path means "look in the conventional locations"; a
// non-empty path that does not exist is an error, because a user who names a
// config file expects it to be used.
func Load(path string) (Config, error) {
	cfg := Default()

	explicit := path != ""
	if !explicit {
		path, explicit = os.LookupEnv(ConfigEnv)
		if path == "" {
			explicit = false
		}
	}
	if !explicit {
		path = discoverFile()
	}
	if path != "" {
		data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied by design
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parsing %s: %w", path, err)
			}
		case explicit || !errors.Is(err, os.ErrNotExist):
			return cfg, fmt.Errorf("reading config: %w", err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ConfigEnv names a config file outright, which is what a container wants: the
// file arrives on a mounted volume at a path the image did not choose, and
// putting it in the environment keeps it out of the command line so that
// overriding args does not silently lose it.
//
// A named file that does not exist is an error, exactly as --config is, because
// somebody who names a config file expects it to be used.
const ConfigEnv = EnvPrefix + "CONFIG"

func discoverFile() string {
	candidates := []string{"veritix.yaml", "veritix.yml"}
	if dir, err := os.UserConfigDir(); err == nil {
		candidates = append(candidates,
			filepath.Join(dir, "veritix", "config.yaml"),
			filepath.Join(dir, "veritix", "config.yml"),
		)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

// applyEnv overlays VERITIX_-prefixed environment variables. Only variables
// that are actually set take effect, so the environment can override a config
// file field-by-field without restating the whole file.
func applyEnv(cfg *Config) {
	str(&cfg.Log.Level, "LOG_LEVEL")
	str(&cfg.Log.Format, "LOG_FORMAT")

	str(&cfg.Server.Addr, "ADDR")
	str(&cfg.Server.AuthToken, "AUTH_TOKEN")
	str(&cfg.Server.DataDir, "DATA_DIR")
	num64(&cfg.Server.MaxUploadBytes, "MAX_UPLOAD_BYTES")
	str(&cfg.Server.SourceURL, "SOURCE_URL")
	dur(&cfg.Server.RetainDatabases, "RETAIN_DATABASES")

	str(&cfg.Engine.MemoryLimit, "ENGINE_MEMORY_LIMIT")
	num(&cfg.Engine.Threads, "ENGINE_THREADS")
	dur(&cfg.Engine.QueryTimeout, "ENGINE_QUERY_TIMEOUT")
	dur(&cfg.Engine.AgentQueryTimeout, "ENGINE_AGENT_QUERY_TIMEOUT")
	num(&cfg.Engine.MaxResultRows, "ENGINE_MAX_RESULT_ROWS")
	str(&cfg.Engine.TempDir, "ENGINE_TEMP_DIR")

	str(&cfg.LLM.Provider, "LLM_PROVIDER")
	str(&cfg.LLM.Model, "LLM_MODEL")
	str(&cfg.LLM.BaseURL, "LLM_BASE_URL")
	str(&cfg.LLM.APIKey, "LLM_API_KEY")
	str(&cfg.LLM.Effort, "LLM_EFFORT")
	boolean(&cfg.LLM.AllowSampleValues, "LLM_ALLOW_SAMPLE_VALUES")
	num(&cfg.LLM.MaxSteps, "LLM_MAX_STEPS")
	num(&cfg.LLM.TokenBudget, "LLM_TOKEN_BUDGET")
	dur(&cfg.LLM.RequestTimeout, "LLM_REQUEST_TIMEOUT")

	boolean(&cfg.Schedule.Enabled, "SCHEDULE_ENABLED")
	dur(&cfg.Schedule.Tick, "SCHEDULE_TICK")

	str(&cfg.Notify.WebhookURL, "NOTIFY_WEBHOOK_URL")
	str(&cfg.Notify.On, "NOTIFY_ON")
	str(&cfg.Notify.MinSeverity, "NOTIFY_MIN_SEVERITY")
	str(&cfg.Notify.Detail, "NOTIFY_DETAIL")
	str(&cfg.Notify.BaseURL, "NOTIFY_BASE_URL")
	dur(&cfg.Notify.Timeout, "NOTIFY_TIMEOUT")

	boolean(&cfg.OTel.Enabled, "OTEL_ENABLED")
	str(&cfg.OTel.Endpoint, "OTEL_ENDPOINT")
	str(&cfg.OTel.ServiceName, "OTEL_SERVICE_NAME")
	dur(&cfg.OTel.ExportTimeout, "OTEL_EXPORT_TIMEOUT")

	// Fall back to each provider's conventional key variable so that a user
	// who already exports ANTHROPIC_API_KEY does not have to restate it.
	if cfg.LLM.APIKey == "" {
		switch cfg.LLM.Provider {
		case ProviderAnthropic:
			cfg.LLM.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		case ProviderOpenAICompatible:
			cfg.LLM.APIKey = os.Getenv("OPENAI_API_KEY")
		}
	}
}

func (n Notify) validate() error {
	// The same refusal server.source_url gets, for a weaker but real reason:
	// this URL is one Veritix posts customer-derived text to, and a scheme
	// that is not http or https is a mistake nobody meant to make.
	if !strings.HasPrefix(n.WebhookURL, "https://") && !strings.HasPrefix(n.WebhookURL, "http://") {
		return fmt.Errorf("notify.webhook_url: want an http or https URL, got %q", n.WebhookURL)
	}
	switch n.On {
	case NotifyOnRegression, NotifyOnFailure, NotifyOnAny:
	default:
		return fmt.Errorf("notify.on: want %q, %q or %q, got %q",
			NotifyOnRegression, NotifyOnFailure, NotifyOnAny, n.On)
	}
	switch n.MinSeverity {
	case "info", "warning", "error":
	default:
		return fmt.Errorf("notify.min_severity: want info, warning or error, got %q", n.MinSeverity)
	}
	switch n.Detail {
	case NotifyDetailFindings, NotifyDetailSummary:
		// ok
	default:
		return fmt.Errorf("notify.detail: want %q or %q, got %q",
			NotifyDetailFindings, NotifyDetailSummary, n.Detail)
	}
	if n.BaseURL != "" &&
		!strings.HasPrefix(n.BaseURL, "https://") && !strings.HasPrefix(n.BaseURL, "http://") {
		return fmt.Errorf("notify.base_url: want an http or https URL, got %q", n.BaseURL)
	}
	if n.Timeout <= 0 {
		return fmt.Errorf("notify.timeout: want a positive duration, got %s", n.Timeout)
	}
	return nil
}

func str(dst *string, key string) {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		*dst = v
	}
}

func num(dst *int, key string) {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

func num64(dst *int64, key string) {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			*dst = n
		}
	}
}

func boolean(dst *bool, key string) {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			*dst = b
		}
	}
}

func dur(dst *time.Duration, key string) {
	if v, ok := os.LookupEnv(EnvPrefix + key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			*dst = d
		}
	}
}

// Validate reports configuration that would fail confusingly later.
func (c Config) Validate() error {
	switch c.Log.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log.level: want one of debug|info|warn|error, got %q", c.Log.Level)
	}
	switch c.Log.Format {
	case "text", "json":
	default:
		return fmt.Errorf("log.format: want text or json, got %q", c.Log.Format)
	}
	switch c.LLM.Provider {
	case ProviderNone, ProviderAnthropic, ProviderOpenAICompatible:
	default:
		return fmt.Errorf("llm.provider: want one of none|anthropic|openai-compatible, got %q", c.LLM.Provider)
	}
	if c.Server.MaxUploadBytes < 1 {
		return fmt.Errorf("server.max_upload_bytes: want a positive size, got %d", c.Server.MaxUploadBytes)
	}
	// The interface renders this as a link's href, so a scheme the browser
	// would execute rather than navigate to is refused here. It is operator
	// configuration rather than untrusted input, but a misconfiguration that
	// turns into script in the one page that can display customer rows is not
	// a category to leave to good intentions.
	if u := c.Server.SourceURL; u != "" &&
		!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("server.source_url: want an http or https URL, got %q", u)
	}
	if c.Server.RetainDatabases < 0 {
		return fmt.Errorf(
			"server.retain_databases: want a duration or 0 to keep everything, got %s",
			c.Server.RetainDatabases)
	}
	if c.Notify.WebhookURL != "" {
		if err := c.Notify.validate(); err != nil {
			return err
		}
	}
	if c.Schedule.Enabled {
		// A tick faster than a second is a busy loop over the run store, and
		// one slower than a few minutes makes "02:00" mean "some time after
		// 02:00" by more than anybody would expect.
		if c.Schedule.Tick < time.Second || c.Schedule.Tick > 10*time.Minute {
			return fmt.Errorf(
				"schedule.tick: want between 1s and 10m, got %s", c.Schedule.Tick)
		}
	}
	if c.Engine.MaxResultRows < 1 {
		return fmt.Errorf("engine.max_result_rows: want a positive count, got %d", c.Engine.MaxResultRows)
	}
	for i, srv := range c.Context.Servers {
		if srv.Name == "" {
			return fmt.Errorf("context.servers[%d]: needs a name, which is how its documents are labeled", i)
		}
		if srv.Command == "" {
			return fmt.Errorf("context.servers[%d] (%s): needs a command to run", i, srv.Name)
		}
		for _, e := range srv.Env {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("context.servers[%d] (%s): env entries are NAME=VALUE, got %q",
					i, srv.Name, e)
			}
		}
	}
	if names := duplicateNames(c.Context.Servers); names != "" {
		return fmt.Errorf("context.servers: %s is configured twice; names label documents "+
			"and have to tell one server from another", names)
	}
	if c.Context.MaxDocumentBytes < 0 {
		return fmt.Errorf("context.max_document_bytes: want a byte count, got %d", c.Context.MaxDocumentBytes)
	}

	if c.LLM.MaxSteps < 1 {
		return fmt.Errorf("llm.max_steps: want a positive count, got %d", c.LLM.MaxSteps)
	}
	// A collector endpoint that is not http or https is a configuration
	// mistake worth catching at startup, rather than an export failure ten
	// minutes into a run that nobody is watching.
	if u := c.OTel.Endpoint; u != "" &&
		!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return fmt.Errorf("otel.endpoint: want an http or https URL, got %q", u)
	}
	if r := c.OTel.SampleRatio; r < 0 || r > 1 {
		return fmt.Errorf("otel.sample_ratio: want a fraction between 0 and 1, got %v", r)
	}
	return nil
}

// IsLoopback reports whether addr binds only to the local machine.
func IsLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	switch host {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return strings.HasPrefix(host, "127.")
}

// duplicateNames reports the first context server name used more than once.
func duplicateNames(servers []ContextServer) string {
	seen := make(map[string]bool, len(servers))
	for _, s := range servers {
		if seen[s.Name] {
			return strconv.Quote(s.Name)
		}
		seen[s.Name] = true
	}
	return ""
}
