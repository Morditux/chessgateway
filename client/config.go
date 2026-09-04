package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/Morditux/chessgateway/internal/protocol"
)

const (
	defaultHost             = "127.0.0.1:9000"
	defaultConnectTimeoutMS = 5000
	defaultMaxLineBytes     = protocol.DefaultMaxLineBytes
	maxConfigBytes          = 1 << 20
)

// Config is the gatewayclient configuration file format (JSON, stored in gatewayclient.conf).
type Config struct {
	Host             string `json:"host"`
	EngineID         string `json:"engine_id"`
	AccessKey        string `json:"access_key,omitempty"`
	ConnectTimeoutMS int    `json:"connect_timeout_ms"`
	MaxLineBytes     int    `json:"max_line_bytes"`
	LogFile          string `json:"log_file,omitempty"`
	// LogCommands opts into logging full UCI command contents. It defaults
	// to false because logs may otherwise retain game data (positions,
	// searches); only command sizes are logged then.
	LogCommands bool       `json:"log_commands,omitempty"`
	TLS         *TLSConfig `json:"tls,omitempty"`
}

// TLSConfig optionally enables TLS for the gateway connection.
type TLSConfig struct {
	Enabled            bool   `json:"enabled"`
	CAFile             string `json:"ca_file,omitempty"`
	CertFile           string `json:"cert_file,omitempty"`
	KeyFile            string `json:"key_file,omitempty"`
	ServerName         string `json:"server_name,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Host:             defaultHost,
		ConnectTimeoutMS: defaultConnectTimeoutMS,
		MaxLineBytes:     defaultMaxLineBytes,
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
	if c.Host == "" {
		return errors.New("host must not be empty")
	}
	if _, port, err := net.SplitHostPort(c.Host); err != nil || port == "" {
		return fmt.Errorf("host must be a host:port address")
	} else if portNumber, err := strconv.ParseUint(port, 10, 16); err != nil || portNumber > 65535 {
		return errors.New("host port must be an integer between 0 and 65535")
	}
	if c.EngineID == "" || len(c.EngineID) > 64 {
		return errors.New("engine_id must be between 1 and 64 characters")
	}
	for _, r := range c.EngineID {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return errors.New("engine_id may contain only ASCII letters, digits, '.', '_' and '-'")
		}
	}
	if c.AccessKey != "" && !isValidUUID(c.AccessKey) {
		return errors.New("access_key must be a valid UUID")
	}
	if c.ConnectTimeoutMS <= 0 || c.ConnectTimeoutMS > 60000 {
		return errors.New("connect_timeout_ms must be between 1 and 60000")
	}
	if c.MaxLineBytes < 1024 || c.MaxLineBytes > 16<<20 {
		return errors.New("max_line_bytes must be between 1024 and 16777216")
	}
	if strings.ContainsRune(c.LogFile, 0) {
		return errors.New("log_file must not contain NUL")
	}
	if c.TLS != nil && c.TLS.Enabled {
		if strings.ContainsRune(c.TLS.CAFile, 0) || strings.ContainsRune(c.TLS.CertFile, 0) || strings.ContainsRune(c.TLS.KeyFile, 0) || strings.ContainsRune(c.TLS.ServerName, 0) {
			return errors.New("tls paths must not contain NUL")
		}
		if (c.TLS.CertFile != "" && c.TLS.KeyFile == "") || (c.TLS.CertFile == "" && c.TLS.KeyFile != "") {
			return errors.New("tls cert_file and key_file must be provided together")
		}
	}
	return nil
}

// isValidUUID reports whether value is a canonical textual UUID (8-4-4-4-12 hex digits).
func isValidUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := 0; index < len(value); index++ {
		ch := value[index]
		switch index {
		case 8, 13, 18, 23:
			if ch != '-' {
				return false
			}
		default:
			switch {
			case ch >= '0' && ch <= '9':
			case ch >= 'a' && ch <= 'f':
			case ch >= 'A' && ch <= 'F':
			default:
				return false
			}
		}
	}
	return true
}
