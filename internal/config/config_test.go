package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("naming a config file that does not exist should be an error")
	}

	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default config must be valid: %v", err)
	}
	if !IsLoopback(cfg.Server.Addr) {
		t.Errorf("default listen address %q must be loopback", cfg.Server.Addr)
	}
	if cfg.LLM.AllowSampleValues {
		t.Error("sending raw cell values to a model must be opt-in")
	}
	if cfg.LLM.Provider != ProviderNone {
		t.Errorf("no LLM provider should be configured by default, got %q", cfg.LLM.Provider)
	}
}

func TestLoadFileThenEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "veritix.yaml")
	body := `
log:
  level: debug
server:
  addr: 0.0.0.0:9999
engine:
  memory_limit: 2GB
  query_timeout: 30s
llm:
  provider: openai-compatible
  model: qwen2.5:14b
  base_url: http://localhost:11434/v1
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want debug", cfg.Log.Level)
	}
	if cfg.Engine.QueryTimeout != 30*time.Second {
		t.Errorf("engine.query_timeout = %v, want 30s", cfg.Engine.QueryTimeout)
	}
	if cfg.LLM.Model != "qwen2.5:14b" {
		t.Errorf("llm.model = %q", cfg.LLM.Model)
	}
	// Fields absent from the file keep their defaults.
	if cfg.Log.Format != "text" {
		t.Errorf("log.format = %q, want the default text", cfg.Log.Format)
	}

	// The environment overrides the file.
	t.Setenv(EnvPrefix+"LOG_LEVEL", "warn")
	t.Setenv(EnvPrefix+"ENGINE_THREADS", "4")
	t.Setenv(EnvPrefix+"LLM_ALLOW_SAMPLE_VALUES", "true")
	t.Setenv(EnvPrefix+"LLM_EFFORT", "none")

	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("env should override the file: log.level = %q", cfg.Log.Level)
	}
	if cfg.Engine.Threads != 4 {
		t.Errorf("engine.threads = %d, want 4", cfg.Engine.Threads)
	}
	if !cfg.LLM.AllowSampleValues {
		t.Error("VERITIX_LLM_ALLOW_SAMPLE_VALUES=true should take effect")
	}
	// "none" is a value Validate must not reject: it is what turns a hybrid
	// reasoning model's thinking off, and every provider spells effort
	// differently.
	if cfg.LLM.Effort != "none" {
		t.Errorf("llm.effort = %q, want none", cfg.LLM.Effort)
	}
}

func TestProviderKeyFallback(t *testing.T) {
	// The explicit variable wins over the fallback, so a developer who exports
	// it in their own shell would otherwise fail this test and nothing else.
	t.Setenv("VERITIX_LLM_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	cfg := Default()
	cfg.LLM.Provider = ProviderAnthropic
	applyEnv(&cfg)
	if cfg.LLM.APIKey != "sk-test" {
		t.Errorf("an already-exported ANTHROPIC_API_KEY should be picked up, got %q", cfg.LLM.APIKey)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"log level":    func(c *Config) { c.Log.Level = "loud" },
		"log format":   func(c *Config) { c.Log.Format = "xml" },
		"provider":     func(c *Config) { c.LLM.Provider = "gpt" },
		"result rows":  func(c *Config) { c.Engine.MaxResultRows = 0 },
		"agent budget": func(c *Config) { c.LLM.MaxSteps = 0 },
		// The interface renders this one as an href.
		"source scheme": func(c *Config) { c.Server.SourceURL = "javascript:alert(1)" },
		// A collector endpoint that cannot be reached is worth refusing at
		// startup rather than ten minutes into a run nobody is watching.
		"otel scheme": func(c *Config) { c.OTel.Endpoint = "collector:4318" },
		"otel ratio":  func(c *Config) { c.OTel.SampleRatio = 1.5 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Error("want an error, got nil")
			}
		})
	}
}

// An empty source URL is a decision, not a mistake: it is how a build shipped
// under the commercial license turns the offer off.
func TestValidateAcceptsNoSourceURL(t *testing.T) {
	cfg := Default()
	cfg.Server.SourceURL = ""
	if err := cfg.Validate(); err != nil {
		t.Errorf("empty server.source_url: %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	loopback := []string{"127.0.0.1:8080", "localhost:8080", "[::1]:8080", ":8080", "127.9.9.9:1"}
	public := []string{"0.0.0.0:8080", "192.168.1.10:8080", "[2001:db8::1]:8080", "example.com:80"}

	for _, addr := range loopback {
		if !IsLoopback(addr) {
			t.Errorf("IsLoopback(%q) = false, want true", addr)
		}
	}
	for _, addr := range public {
		if IsLoopback(addr) {
			t.Errorf("IsLoopback(%q) = true, want false", addr)
		}
	}
}

// TestConfigEnvNamesTheFile covers what a container needs: the config arrives
// on a mounted volume at a path the image did not choose, so it is named in the
// environment rather than on the command line, where overriding args would
// silently lose it.
func TestConfigEnvNamesTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  level: warn\notel:\n  enabled: true\n  endpoint: http://collector:4318\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(ConfigEnv, path)
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("loading the file named by %s: %v", ConfigEnv, err)
	}
	if cfg.Log.Level != "warn" {
		t.Errorf("log.level = %q, want warn", cfg.Log.Level)
	}
	if !cfg.OTel.Enabled || cfg.OTel.Endpoint != "http://collector:4318" {
		t.Errorf("otel = %+v, want the file's settings", cfg.OTel)
	}

	// A named file that is not there is an error, exactly as --config is:
	// somebody who names a config file expects it to be used.
	t.Setenv(ConfigEnv, filepath.Join(dir, "absent.yaml"))
	if _, err := Load(""); err == nil {
		t.Error("a missing file named by the environment was accepted")
	}

	// An explicit --config still wins, so the flag is not shadowed by an
	// environment somebody set once and forgot.
	other := filepath.Join(dir, "other.yaml")
	if err := os.WriteFile(other, []byte("log:\n  level: error\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(other)
	if err != nil {
		t.Fatalf("loading an explicit path: %v", err)
	}
	if cfg.Log.Level != "error" {
		t.Errorf("--config lost to %s: log.level = %q", ConfigEnv, cfg.Log.Level)
	}
}

// Nothing talks to anybody until it is told to. Two switches, one rule.
func TestNothingIsEnabledByDefault(t *testing.T) {
	cfg := Default()
	if cfg.LLM.Provider != ProviderNone {
		t.Errorf("llm.provider = %q, want %q", cfg.LLM.Provider, ProviderNone)
	}
	if cfg.OTel.Enabled {
		t.Error("otel.enabled is true by default")
	}
}
