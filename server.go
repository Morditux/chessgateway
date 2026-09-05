package gateway

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

// Server accepts one independent engine session per client connection.
type Server struct {
	config  Config
	logger  *log.Logger
	clients chan struct{}
	engines map[string]EngineConfig
	auth    *accessKeyStore
}

func NewServer(config Config, logger *log.Logger) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = log.Default()
	}
	engines := make(map[string]EngineConfig, len(config.Engines))
	for _, engine := range config.Engines {
		engines[engine.ID] = engine
	}
	var store *accessKeyStore
	if config.Auth != nil && config.Auth.Enabled {
		entries, err := loadClientsFile(config.Auth.ClientsFile)
		if err != nil {
			return nil, err
		}
		warnClientsFilePerms(config.Auth.ClientsFile, logger)
		store = newAccessKeyStore(entries)
	}
	return &Server{
		config:  config,
		logger:  logger,
		clients: make(chan struct{}, config.MaxClients),
		engines: engines,
		auth:    store,
	}, nil
}

// ListenAndServe opens the configured TCP listener. A TLS listener is used if
// the configuration contains a tls section.
func (s *Server) ListenAndServe(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.config.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.config.Listen, err)
	}
	if s.config.TLS != nil {
		certificate, err := tls.LoadX509KeyPair(s.config.TLS.CertFile, s.config.TLS.KeyFile)
		if err != nil {
			_ = listener.Close()
			return fmt.Errorf("load TLS certificate: %w", err)
		}
		listener = tls.NewListener(listener, &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS13,
		})
	}
	return s.Serve(ctx, listener)
}

// Serve serves an already-created listener. It is public so callers and tests
// can choose an ephemeral address or provide a listener with their own policy.
func (s *Server) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("listener must not be nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	contextDone := make(chan struct{})
	var activeMu sync.Mutex
	activeConnections := make(map[net.Conn]struct{})
	closeActiveConnections := func() {
		activeMu.Lock()
		defer activeMu.Unlock()
		for connection := range activeConnections {
			_ = connection.Close()
		}
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = listener.Close()
			closeActiveConnections()
		case <-contextDone:
		}
	}()
	defer close(contextDone)
	defer listener.Close()
	defer closeActiveConnections()
	var clients sync.WaitGroup
	shutdown := func(err error) error {
		closeActiveConnections()
		clients.Wait()
		return err
	}

	for {
		connection, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return shutdown(nil)
			default:
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				// Transient shortage (fd pressure surfaces as timeouts on
				// some platforms): back off briefly instead of aborting.
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return shutdown(fmt.Errorf("accept client: %w", err))
		}

		select {
		case s.clients <- struct{}{}:
			activeMu.Lock()
			activeConnections[connection] = struct{}{}
			activeMu.Unlock()
			clients.Add(1)
			go func() {
				defer clients.Done()
				defer func() {
					activeMu.Lock()
					delete(activeConnections, connection)
					activeMu.Unlock()
				}()
				s.handleClient(connection)
			}()
		default:
			// Do not let an unbounded number of connections consume goroutines or
			// engine processes. The short error is best effort.
			_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
			_ = writeJSON(connection, Response{Type: "error", Code: "server_busy", Message: "maximum clients reached"})
			_ = connection.Close()
		}
	}
}

type client struct {
	connection    net.Conn
	writeMu       sync.Mutex
	sendQueue     chan Response
	done          chan struct{}
	closeOnce     sync.Once
	session       *engineSession
	server        *Server
	authenticated bool
	lastSelect    time.Time
}

// clientSendQueueSize bounds the number of asynchronous engine events
// (uci_output, engine_exited) buffered for a slow client. Synchronous
// responses bypass the queue and are written directly, so control frames keep
// their delivery guarantee; the queue absorbs engine output bursts and a
// client that never reads backpressures the engine reader once it is full,
// until the per-write deadline drops the connection.
const clientSendQueueSize = 256

func (s *Server) handleClient(connection net.Conn) {
	defer func() {
		<-s.clients
		_ = connection.Close()
	}()

	client := &client{
		connection: connection,
		server:     s,
		sendQueue:  make(chan Response, clientSendQueueSize),
		done:       make(chan struct{}),
	}
	client.session = newEngineSession(s.config, s.logger, client.engineOutput, client.engineExited)
	go client.writeLoop()
	defer client.closeDone()
	// This also bounds a TLS handshake performed lazily by tls.Conn.Write.
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	features := []string{"engine_list", "engine_selection", "engine_stop", "uci_stream"}
	if s.auth != nil {
		features = append(features, "access_keys")
	}
	if err := client.write(Response{
		Type:     "hello",
		Protocol: ProtocolName,
		Features: features,
	}); err != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})

	// A client that never sends its first frame should not occupy a slot
	// forever. Once the first valid request arrives, UCI searches may be long;
	// with authentication enabled the deadline is only lifted once the client
	// has authenticated.
	_ = connection.SetReadDeadline(time.Now().Add(30 * time.Second))
	defer func() {
		client.closeDone()
		client.session.stop()
	}()
	reader := bufio.NewReaderSize(connection, 64<<10)
	firstRequest := true
	for {
		line, err := readLine(reader, s.config.MaxLineBytes)
		if errors.Is(err, errLineTooLong) {
			if client.writeError("", "line_too_long", "JSON line exceeds the configured limit") != nil {
				return
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.logger.Printf("client read: %v", err)
			}
			return
		}
		var request Request
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			if client.writeError("", "invalid_json", "request is not valid JSON") != nil {
				return
			}
			continue
		}
		if err := validateRequest(request); err != nil {
			if client.writeError(request.RequestID, "invalid_request", err.Error()) != nil {
				return
			}
			continue
		}
		if firstRequest {
			firstRequest = false
			if s.auth == nil {
				_ = connection.SetReadDeadline(time.Time{})
			}
		}
		if !client.handleRequest(request) {
			return
		}
	}
}

func (c *client) handleRequest(request Request) bool {
	if c.server.auth != nil && !c.authenticated {
		return c.handleUnauthenticatedRequest(request)
	}
	switch request.Type {
	case "authenticate":
		return c.writeError(request.RequestID, "already_authenticated", "this connection is already authenticated") == nil

	case "list_engines":
		engines := make([]EngineInfo, 0, len(c.server.config.Engines))
		for _, engine := range c.server.config.Engines {
			engines = append(engines, engine.Info())
		}
		return c.write(Response{Type: "engines", RequestID: request.RequestID, Engines: engines}) == nil

	case "select_engine":
		engine, ok := c.server.engines[request.EngineID]
		if !ok {
			return c.writeError(request.RequestID, "unknown_engine", "requested engine is not configured") == nil
		}
		// Selecting the already-attached engine is a no-op: it must not kill
		// a running search, and it bypasses the restart cooldown below.
		if c.session.currentID() == engine.ID {
			info := engine.Info()
			return c.write(Response{Type: "engine_selected", RequestID: request.RequestID, EngineID: engine.ID, Engine: &info}) == nil
		}
		// Bound how fast one connection may restart engine processes so a
		// client cannot churn fork/exec in a tight loop. The cooldown is
		// armed by every attempt that reaches the start stage, including a
		// failed start (which still forked); unknown engine ids are cheap
		// and never arm it.
		if interval := c.server.config.minSelectInterval(); interval > 0 {
			if since := time.Since(c.lastSelect); since < interval {
				return c.writeError(request.RequestID, "select_too_frequent", "wait before selecting another engine") == nil
			}
		}
		selectErr := c.session.selectEngine(engine)
		c.lastSelect = time.Now()
		if selectErr != nil {
			return c.writeError(request.RequestID, "engine_start_failed", "selected engine could not be started") == nil
		}
		info := engine.Info()
		return c.write(Response{Type: "engine_selected", RequestID: request.RequestID, EngineID: engine.ID, Engine: &info}) == nil

	case "stop_engine":
		engineID := c.session.stop()
		return c.write(Response{Type: "engine_stopped", RequestID: request.RequestID, EngineID: engineID}) == nil

	case "uci":
		if err := validateUCICommand(request.Command); err != nil {
			return c.writeError(request.RequestID, "invalid_uci_command", err.Error()) == nil
		}
		if err := c.session.send(request.Command); err != nil {
			code := "engine_command_failed"
			message := "command could not be sent to the engine"
			if errors.Is(err, errNoEngine) {
				code = "no_engine_selected"
				message = "select an engine before sending UCI commands"
			}
			return c.writeError(request.RequestID, code, message) == nil
		}
		return true

	default:
		return c.writeError(request.RequestID, "unknown_request_type", "unsupported request type") == nil
	}
}

// handleUnauthenticatedRequest only accepts the authenticate frame. Any other
// request is refused until the client presents a valid access key; a failed
// attempt is answered once and the connection is closed to bound guessing.
func (c *client) handleUnauthenticatedRequest(request Request) bool {
	if request.Type != "authenticate" {
		return c.writeError(request.RequestID, "authentication_required", "send an authenticate request with a valid access key") == nil
	}
	name, ok := c.server.auth.authenticate(request.AccessKey)
	if !ok {
		c.server.logger.Printf("client %s presented an invalid access key", c.connection.RemoteAddr())
		_ = c.writeError(request.RequestID, "invalid_access_key", "the provided access key is not valid")
		return false
	}
	c.authenticated = true
	_ = c.connection.SetReadDeadline(time.Time{})
	c.server.logger.Printf("client %s authenticated as %q", c.connection.RemoteAddr(), name)
	return c.write(Response{Type: "authenticated", RequestID: request.RequestID}) == nil
}

func (c *client) engineOutput(engineID, line string, stopping <-chan struct{}) {
	select {
	case c.sendQueue <- Response{Type: "uci_output", EngineID: engineID, Line: line}:
	case <-stopping:
	case <-c.done:
	}
}

func (c *client) engineExited(engineID string) {
	_ = c.enqueue(Response{Type: "engine_exited", EngineID: engineID})
}

func (c *client) writeError(requestID, code, message string) error {
	return c.write(Response{Type: "error", RequestID: requestID, Code: code, Message: message})
}

// write sends a synchronous response (hello, acks, errors) directly. It
// completes before handleRequest returns, so a final error is always
// delivered before the connection is torn down.
func (c *client) write(response Response) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.connection.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := writeJSON(c.connection, response)
	_ = c.connection.SetWriteDeadline(time.Time{})
	if err != nil {
		_ = c.connection.Close()
	}
	return err
}

// enqueue appends an engine event to the writer queue. It blocks while a slow
// client drains the bounded queue and reports net.ErrClosed once the
// connection is being torn down, so engine readers never panic on a closed
// channel and never lose lines silently while the client is alive.
func (c *client) enqueue(response Response) error {
	select {
	case c.sendQueue <- response:
		return nil
	case <-c.done:
		return net.ErrClosed
	}
}

func (c *client) closeDone() {
	c.closeOnce.Do(func() { close(c.done) })
}

// writeLoop is the only writer of queued engine events. It shares writeMu
// with synchronous responses so frames never interleave on the wire.
func (c *client) writeLoop() {
	for {
		select {
		case response := <-c.sendQueue:
			c.writeMu.Lock()
			_ = c.connection.SetWriteDeadline(time.Now().Add(30 * time.Second))
			err := writeJSON(c.connection, response)
			_ = c.connection.SetWriteDeadline(time.Time{})
			c.writeMu.Unlock()
			if err != nil {
				_ = c.connection.Close()
				return
			}
		case <-c.done:
			return
		}
	}
}
