package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Morditux/chessgateway/internal/protocol"
)

// The wire types are aliased from internal/protocol, shared with the server
// so the two sides cannot drift.
type (
	Request    = protocol.Request
	EngineInfo = protocol.EngineInfo
	Response   = protocol.Response
)

const ProtocolName = protocol.ProtocolName

var errLineTooLong = protocol.ErrLineTooLong

// dialGatewayFunc dials the gateway. It is a variable (not a direct call) so
// tests can substitute an in-memory pipe for the TCP/TLS connection.
var dialGatewayFunc = dialGateway

func readLine(reader *bufio.Reader, maxBytes int) (string, error) {
	return protocol.ReadLine(reader, maxBytes)
}

func writeJSON(writer io.Writer, value any) error {
	return protocol.WriteJSON(writer, value)
}

func containsControlSeparator(value string) bool {
	return protocol.ContainsControlSeparator(value)
}

// Run connects to the gateway and bridges UCI stdin/stdout.
// stdin is the UCI input from the GUI, stdout is the UCI output to the GUI.
func Run(ctx context.Context, cfg Config, stdin io.Reader, stdout io.Writer, logger *log.Logger) error {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	conn, err := dialGatewayFunc(cfg, logger)
	if err != nil {
		return fmt.Errorf("dial gateway: %w", err)
	}
	defer conn.Close()

	reader := bufio.NewReaderSize(conn, 64<<10)
	var writeMu sync.Mutex

	writeRequest := func(req Request) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		err := writeJSON(conn, req)
		_ = conn.SetWriteDeadline(time.Time{})
		return err
	}

	// Handshake: hello
	_ = conn.SetReadDeadline(time.Now().Add(time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond))
	helloLine, err := readLine(reader, cfg.MaxLineBytes)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	var hello Response
	if err := json.Unmarshal([]byte(helloLine), &hello); err != nil {
		return fmt.Errorf("decode hello: %w", err)
	}
	if hello.Type != "hello" {
		return fmt.Errorf("expected hello, got %q", hello.Type)
	}
	if hello.Protocol != ProtocolName {
		return fmt.Errorf("unsupported protocol %q", hello.Protocol)
	}
	features := make(map[string]struct{}, len(hello.Features))
	for _, f := range hello.Features {
		features[f] = struct{}{}
	}
	_, needAuth := features["access_keys"]
	logger.Printf("connected to %s protocol %s features %v", cfg.Host, hello.Protocol, hello.Features)

	// Authenticate if required.
	if needAuth {
		if cfg.AccessKey == "" {
			return errors.New("server requires authentication but no access_key is configured")
		}
		if err := writeRequest(Request{Type: "authenticate", RequestID: "auth", AccessKey: cfg.AccessKey}); err != nil {
			return fmt.Errorf("send authenticate: %w", err)
		}
		// Wait for authenticated or error.
		for {
			line, err := readLine(reader, cfg.MaxLineBytes)
			if err != nil {
				return fmt.Errorf("read auth response: %w", err)
			}
			var resp Response
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				logger.Printf("invalid json during auth: %q", line)
				continue
			}
			if resp.Type == "authenticated" {
				logger.Printf("authenticated to gateway")
				break
			}
			if resp.Type == "error" {
				return fmt.Errorf("authentication failed [%s]: %s", resp.Code, resp.Message)
			}
			logger.Printf("unexpected message during auth: %s", line)
		}
	} else {
		if cfg.AccessKey != "" {
			logger.Printf("access_key configured but server does not require authentication; ignoring")
		}
	}

	// Select engine.
	if err := writeRequest(Request{Type: "select_engine", RequestID: "select", EngineID: cfg.EngineID}); err != nil {
		return fmt.Errorf("send select_engine: %w", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond))
	for {
		line, err := readLine(reader, cfg.MaxLineBytes)
		if err != nil {
			return fmt.Errorf("read select_engine response: %w", err)
		}
		var resp Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			logger.Printf("invalid json during select: %q", line)
			continue
		}
		if resp.Type == "engine_selected" {
			if resp.Engine != nil {
				logger.Printf("engine selected: %s (%s %s)", resp.Engine.ID, resp.Engine.Name, resp.Engine.Version)
			} else {
				logger.Printf("engine selected: %s", resp.EngineID)
			}
			break
		}
		if resp.Type == "error" {
			return fmt.Errorf("select_engine failed [%s]: %s", resp.Code, resp.Message)
		}
		// Other messages should not appear before selection, but ignore.
		logger.Printf("unexpected message during select: %s", line)
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetDeadline(time.Time{})

	logger.Printf("gateway ready, bridging UCI (engine %s)", cfg.EngineID)

	// Bridge phase.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Close connection when context is cancelled.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	errCh := make(chan error, 2)
	// Gateway -> stdout
	go func() {
		defer cancel()
		for {
			line, err := readLine(reader, cfg.MaxLineBytes)
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "closed") {
					errCh <- nil
					return
				}
				if errors.Is(err, errLineTooLong) {
					logger.Printf("gateway line too long, discarded")
					continue
				}
				// Check context cancelled
				select {
				case <-ctx.Done():
					errCh <- nil
					return
				default:
				}
				logger.Printf("gateway read: %v", err)
				errCh <- err
				return
			}
			var resp Response
			if err := json.Unmarshal([]byte(line), &resp); err != nil {
				logger.Printf("invalid gateway json: %q: %v", line, err)
				continue
			}
			switch resp.Type {
			case "uci_output":
				// Relay verbatim to GUI stdout.
				if _, err := io.WriteString(stdout, resp.Line+"\n"); err != nil {
					logger.Printf("stdout write: %v", err)
					errCh <- err
					return
				}
				// Flush if stdout is a flusher (e.g., bufio.Writer)
				if fl, ok := stdout.(interface{ Flush() error }); ok {
					_ = fl.Flush()
				}
			case "error":
				// Structured error from gateway. Log it. Optionally forward as info string to GUI.
				logger.Printf("gateway error [%s]: %s (request %s)", resp.Code, resp.Message, resp.RequestID)
				// Forward to GUI as info string so it appears in GUI logs, but do not break UCI.
				// This is harmless: UCI 'info string' is ignored for move logic.
				// We avoid leaking executable paths (gateway already sanitizes messages).
				msg := fmt.Sprintf("info string gateway error [%s]: %s", resp.Code, resp.Message)
				_, _ = io.WriteString(stdout, msg+"\n")
				if fl, ok := stdout.(interface{ Flush() error }); ok {
					_ = fl.Flush()
				}
			case "engine_exited":
				// The attached engine died on its own: surface it to the GUI
				// as an ignorable info string (like gateway errors) and keep
				// bridging; the user re-selects via a reconnect.
				logger.Printf("gateway engine %s exited", resp.EngineID)
				_, _ = io.WriteString(stdout, fmt.Sprintf("info string gateway engine %s exited\n", resp.EngineID))
				if fl, ok := stdout.(interface{ Flush() error }); ok {
					_ = fl.Flush()
				}
			case "hello", "authenticated", "engines", "engine_selected", "engine_stopped":
				logger.Printf("gateway control message: %s", line)
			default:
				logger.Printf("unknown gateway message type %q: %s", resp.Type, line)
			}
		}
	}()

	// stdin (GUI) -> gateway
	go func() {
		defer cancel()
		bufReader := bufio.NewReaderSize(stdin, 64<<10)
		for {
			select {
			case <-ctx.Done():
				errCh <- nil
				return
			default:
			}
			line, err := readLine(bufReader, cfg.MaxLineBytes)
			if err != nil {
				if errors.Is(err, io.EOF) {
					logger.Printf("stdin EOF")
					// Try to quit engine gracefully.
					_ = writeRequest(Request{Type: "uci", Command: "quit"})
					errCh <- nil
					return
				}
				if errors.Is(err, errLineTooLong) {
					logger.Printf("stdin line too long, discarded")
					continue
				}
				// Network closed or other error
				if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "closed") {
					errCh <- nil
					return
				}
				select {
				case <-ctx.Done():
					errCh <- nil
					return
				default:
				}
				logger.Printf("stdin read: %v", err)
				errCh <- err
				return
			}
			// Preserve original line for UCI; readLine already stripped CRLF.
			// Validate no control separators remain (should not happen as readLine splits).
			if containsControlSeparator(line) {
				logger.Printf("stdin command contains control separator, discarded: %q", line)
				continue
			}
			// Empty lines are skipped locally; the gateway also rejects empty
			// commands with invalid_uci_command. GUI typically does not send
			// empty lines; ignore them to avoid spurious traffic.
			if strings.TrimSpace(line) == "" {
				continue
			}
			if err := writeRequest(Request{Type: "uci", Command: line}); err != nil {
				logger.Printf("gateway write: %v", err)
				errCh <- err
				return
			}
			if cfg.LogCommands {
				logger.Printf("uci -> gateway: %q", line)
			} else {
				logger.Printf("uci command forwarded: %d bytes", len(line))
			}
			// If GUI sent quit, initiate graceful shutdown.
			if strings.TrimSpace(line) == "quit" {
				// Give gateway a moment to propagate quit and engine output.
				time.Sleep(100 * time.Millisecond)
				errCh <- nil
				return
			}
		}
	}()

	// Wait for either direction to finish.
	// We consider the first error; if one side exits normally, we give the other a grace period then exit.
	select {
	case <-ctx.Done():
		// Context cancelled externally
		// Wait briefly for goroutines
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return nil
		}
	case err := <-errCh:
		// One goroutine finished, cancel and wait for the other
		cancel()
		// Wait for second goroutine with timeout
		select {
		case err2 := <-errCh:
			if err != nil {
				return err
			}
			return err2
		case <-time.After(2 * time.Second):
			if err != nil {
				return err
			}
			return nil
		}
	}
}

func dialGateway(cfg Config, logger *log.Logger) (net.Conn, error) {
	timeout := time.Duration(cfg.ConnectTimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	address := cfg.Host

	// Plain TCP if TLS not enabled.
	if cfg.TLS == nil || !cfg.TLS.Enabled {
		dialer := net.Dialer{Timeout: timeout}
		conn, err := dialer.Dial("tcp", address)
		if err != nil {
			return nil, err
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}

	// TLS mode.
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: cfg.TLS.InsecureSkipVerify,
	}
	if cfg.TLS.ServerName != "" {
		tlsCfg.ServerName = cfg.TLS.ServerName
	} else {
		// Use host without port as SNI server name.
		if host, _, err := net.SplitHostPort(address); err == nil {
			tlsCfg.ServerName = host
		}
	}
	if cfg.TLS.CAFile != "" {
		caData, err := os.ReadFile(cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caData) {
			return nil, fmt.Errorf("failed to parse ca_file %q", cfg.TLS.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.TLS.CertFile != "" || cfg.TLS.KeyFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", address, tlsCfg)
	if err != nil {
		return nil, err
	}
	if logger != nil {
		state := conn.ConnectionState()
		logger.Printf("TLS handshake: version %x cipher %x server %s", state.Version, state.CipherSuite, tlsCfg.ServerName)
	}
	return conn, nil
}
