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
}

// Log controls diagnostic output.
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string `yaml:"level"`
	// Format is text (human-readable) or json (machine-readable).
	Format string `yaml:"format"`
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
	// QueryTimeout bounds any single query.
	QueryTimeout time.Duration `yaml:"query_timeout"`
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
			ReadHeaderTimeout: 10 * time.Second,
			ShutdownTimeout:   15 * time.Second,
		},
		Engine: Engine{
			QueryTimeout:  2 * time.Minute,
			MaxResultRows: 10_000,
		},
		LLM: LLM{
			Provider:       ProviderNone,
			MaxSteps:       40,
			RequestTimeout: 10 * time.Minute,
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

	str(&cfg.Engine.MemoryLimit, "ENGINE_MEMORY_LIMIT")
	num(&cfg.Engine.Threads, "ENGINE_THREADS")
	dur(&cfg.Engine.QueryTimeout, "ENGINE_QUERY_TIMEOUT")
	num(&cfg.Engine.MaxResultRows, "ENGINE_MAX_RESULT_ROWS")
	str(&cfg.Engine.TempDir, "ENGINE_TEMP_DIR")

	str(&cfg.LLM.Provider, "LLM_PROVIDER")
	str(&cfg.LLM.Model, "LLM_MODEL")
	str(&cfg.LLM.BaseURL, "LLM_BASE_URL")
	str(&cfg.LLM.APIKey, "LLM_API_KEY")
	boolean(&cfg.LLM.AllowSampleValues, "LLM_ALLOW_SAMPLE_VALUES")
	num(&cfg.LLM.MaxSteps, "LLM_MAX_STEPS")
	num(&cfg.LLM.TokenBudget, "LLM_TOKEN_BUDGET")
	dur(&cfg.LLM.RequestTimeout, "LLM_REQUEST_TIMEOUT")

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
	if c.Engine.MaxResultRows < 1 {
		return fmt.Errorf("engine.max_result_rows: want a positive count, got %d", c.Engine.MaxResultRows)
	}
	if c.LLM.MaxSteps < 1 {
		return fmt.Errorf("llm.max_steps: want a positive count, got %d", c.LLM.MaxSteps)
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
