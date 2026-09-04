package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddress   = "127.0.0.1:9000"
	defaultMaxClients      = 64
	defaultShutdownTimeout = 2 * time.Second
	// defaultMinSelectInterval bounds how fast one connection may restart
	// engine processes via select_engine. It stops a client from churning
	// fork/exec in a tight loop; selecting the already-attached engine is
	// a no-op and bypasses the cooldown.
	defaultMinSelectInterval = 500 * time.Millisecond
	maxConfigBytes           = 4 << 20 // Configuration is local input, but need not be unbounded.
)

// Config is the server configuration file format.
type Config struct {
	Listen              string         `json:"listen"`
	MaxClients          int            `json:"max_clients"`
	MaxLineBytes        int            `json:"max_line_bytes"`
	ShutdownTimeoutMS   int            `json:"shutdown_timeout_ms"`
	MinSelectIntervalMS int            `json:"min_select_interval_ms"`
	TLS                 *TLSConfig     `json:"tls,omitempty"`
	Auth                *AuthConfig    `json:"auth,omitempty"`
	Engines             []EngineConfig `json:"engines"`
}

// TLSConfig enables TLS when both files are configured. The server does not
// invent authentication: deployments that expose it beyond a trusted network
// should use TLS plus an authenticated tunnel or an upstream authenticator.
type TLSConfig struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

// AuthConfig optionally requires clients to authenticate with the UUID access
// keys listed in ClientsFile before any other request is accepted.
type AuthConfig struct {
	Enabled     bool   `json:"enabled"`
	ClientsFile string `json:"clients_file"`
}

// EngineConfig describes an executable that the server administrator allows
// clients to use. Args are passed directly to os/exec; they are never parsed
// as shell text.
type EngineConfig struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Listen:              defaultListenAddress,
		MaxClients:          defaultMaxClients,
		MaxLineBytes:        defaultMaxLineBytes,
		ShutdownTimeoutMS:   int(defaultShutdownTimeout / time.Millisecond),
		MinSelectIntervalMS: int(defaultMinSelectInterval / time.Millisecond),
	}
}

func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config: %w", err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return Config{}, fmt.Errorf("stat config: %w", err)
	} else if info.Size() > maxConfigBytes {
		return Config{}, errors.New("config exceeds the maximum size")
	}

	config := DefaultConfig()
	decoder := json.NewDecoder(io.LimitReader(file, maxConfigBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("config contains more than one JSON value")
		}
		return Config{}, fmt.Errorf("read config tail: %w", err)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("listen must not be empty")
	}
	if _, port, err := net.SplitHostPort(c.Listen); err != nil || port == "" {
		return fmt.Errorf("listen must be a host:port address")
	} else if portNumber, err := strconv.ParseUint(port, 10, 16); err != nil || portNumber > 65535 {
		return errors.New("listen port must be an integer between 0 and 65535")
	}
	if c.MaxClients <= 0 || c.MaxClients > 4096 {
		return errors.New("max_clients must be between 1 and 4096")
	}
	if c.MaxLineBytes < 1024 || c.MaxLineBytes > 16<<20 {
		return errors.New("max_line_bytes must be between 1024 and 16777216")
	}
	if c.ShutdownTimeoutMS <= 0 || c.ShutdownTimeoutMS > 60000 {
		return errors.New("shutdown_timeout_ms must be between 1 and 60000")
	}
	if c.MinSelectIntervalMS < 0 || c.MinSelectIntervalMS > 60000 {
		return errors.New("min_select_interval_ms must be between 0 and 60000")
	}

	seen := make(map[string]struct{}, len(c.Engines))
	for index, engine := range c.Engines {
		if err := validateEngine(engine); err != nil {
			return fmt.Errorf("engines[%d]: %w", index, err)
		}
		if _, exists := seen[engine.ID]; exists {
			return fmt.Errorf("duplicate engine id %q", engine.ID)
		}
		seen[engine.ID] = struct{}{}
	}

	if c.TLS != nil {
		if c.TLS.CertFile == "" || c.TLS.KeyFile == "" {
			return errors.New("tls requires both cert_file and key_file")
		}
		if strings.ContainsRune(c.TLS.CertFile, 0) || strings.ContainsRune(c.TLS.KeyFile, 0) {
			return errors.New("tls paths must not contain NUL")
		}
	}

	if c.Auth != nil && c.Auth.Enabled {
		if c.Auth.ClientsFile == "" {
			return errors.New("auth.enabled requires clients_file")
		}
		if strings.ContainsRune(c.Auth.ClientsFile, 0) {
			return errors.New("auth.clients_file must not contain NUL")
		}
	}
	return nil
}

func validateEngine(engine EngineConfig) error {
	if engine.ID == "" || len(engine.ID) > 64 {
		return errors.New("id must be between 1 and 64 characters")
	}
	for _, r := range engine.ID {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return errors.New("id may contain only ASCII letters, digits, '.', '_' and '-'")
		}
	}
	if engine.Name == "" || len(engine.Name) > 256 {
		return errors.New("name must be between 1 and 256 characters")
	}
	if engine.Version == "" || len(engine.Version) > 128 {
		return errors.New("version must be between 1 and 128 characters")
	}
	if engine.Command == "" || strings.ContainsRune(engine.Command, 0) {
		return errors.New("command must not be empty or contain NUL")
	}
	if len(engine.Args) > 128 {
		return errors.New("args may contain at most 128 entries")
	}
	for _, arg := range engine.Args {
		if strings.ContainsRune(arg, 0) {
			return errors.New("args must not contain NUL")
		}
	}
	return nil
}

func (c Config) shutdownTimeout() time.Duration {
	return time.Duration(c.ShutdownTimeoutMS) * time.Millisecond
}

func (c Config) minSelectInterval() time.Duration {
	if c.MinSelectIntervalMS < 0 {
		return 0
	}
	return time.Duration(c.MinSelectIntervalMS) * time.Millisecond
}

func (e EngineConfig) Info() EngineInfo {
	return EngineInfo{ID: e.ID, Name: e.Name, Version: e.Version}
}
