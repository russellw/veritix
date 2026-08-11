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
}

func TestProviderKeyFallback(t *testing.T) {
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
