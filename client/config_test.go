package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultConfigIsValid(t *testing.T) {
	config := DefaultConfig()
	config.EngineID = "stockfish-17"
	if err := config.Validate(); err != nil {
		t.Fatalf("DefaultConfig rejected: %v", err)
	}
}

func TestClientConfigValidates(t *testing.T) {
	valid := DefaultConfig()
	valid.EngineID = "stockfish-17"
	cases := map[string]func(*Config){
		"empty host":            func(c *Config) { c.Host = "" },
		"host without port":     func(c *Config) { c.Host = "127.0.0.1" },
		"bad port":              func(c *Config) { c.Host = "127.0.0.1:notaport" },
		"empty engine":          func(c *Config) { c.EngineID = "" },
		"engine too long":       func(c *Config) { c.EngineID = strings.Repeat("a", 65) },
		"engine forbidden char": func(c *Config) { c.EngineID = "stock fish" },
		"bad access key":        func(c *Config) { c.AccessKey = "not-a-uuid" },
		"timeout zero":          func(c *Config) { c.ConnectTimeoutMS = 0 },
		"timeout too large":     func(c *Config) { c.ConnectTimeoutMS = 60001 },
		"line limit too small":  func(c *Config) { c.MaxLineBytes = 512 },
		"nul log file":          func(c *Config) { c.LogFile = "a\x00b" },
		"cert without key": func(c *Config) {
			c.TLS = &TLSConfig{Enabled: true, CertFile: "client.crt"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestLoadClientConfig(t *testing.T) {
	content := `{
		"host": "10.0.0.2:9000",
		"engine_id": "stockfish-17",
		"access_key": "f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
		"connect_timeout_ms": 2000,
		"max_line_bytes": 65536,
		"log_commands": true
	}`
	path := filepath.Join(t.TempDir(), "gatewayclient.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Host != "10.0.0.2:9000" || config.EngineID != "stockfish-17" || !config.LogCommands {
		t.Fatalf("config = %+v", config)
	}

	if _, err := LoadConfig(filepath.Join(t.TempDir(), "absent.conf")); err == nil {
		t.Fatal("missing config accepted")
	}
	unknown := filepath.Join(t.TempDir(), "unknown.conf")
	if err := os.WriteFile(unknown, []byte(`{"host":"x:1","engine_id":"e","unknown_field":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(unknown); err == nil {
		t.Fatal("unknown field accepted")
	}
}
