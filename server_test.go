package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestUCIExitHelperProcess exits immediately without speaking UCI. It is used
// as a crashing engine subprocess: selecting it must produce engine_exited.
func TestUCIExitHelperProcess(t *testing.T) {
	if os.Getenv("CHESSGATEWAY_HELPER_EXIT") != "1" {
		return
	}
	os.Exit(1)
}

// TestUCIHelperProcess is also used as a tiny UCI engine subprocess by the
// integration test. It deliberately implements only the commands needed by
// the test; the gateway itself does not whitelist UCI commands.
func TestUCIHelperProcess(t *testing.T) {
	if os.Getenv("CHESSGATEWAY_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "uci":
			_, _ = fmt.Fprintln(os.Stdout, "id name Gateway Test Engine")
			_, _ = fmt.Fprintln(os.Stdout, "id author chessgateway")
			_, _ = fmt.Fprintln(os.Stdout, "uciok")
		case "isready":
			_, _ = fmt.Fprintln(os.Stdout, "readyok")
		case "go":
			_, _ = fmt.Fprintln(os.Stdout, "info depth 1 score cp 12")
			_, _ = fmt.Fprintln(os.Stdout, "bestmove e2e4")
		case "noisy":
			// Engines log to stderr; the gateway must drain it without
			// leaking it into the client UCI stream.
			_, _ = fmt.Fprintln(os.Stderr, "engine log line")
			_, _ = fmt.Fprintln(os.Stdout, "readyok")
		case "stop":
			_, _ = fmt.Fprintln(os.Stdout, "bestmove 0000")
		case "quit":
			return
		default:
			if strings.HasPrefix(scanner.Text(), "go ") {
				_, _ = fmt.Fprintln(os.Stdout, "info depth 1 score cp 12")
				_, _ = fmt.Fprintln(os.Stdout, "bestmove e2e4")
			}
		}
	}
}

func TestReadLineLimit(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("short\nthis line is too long\n"))
	line, err := readLine(reader, 10)
	if err != nil || line != "short" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	_, err = readLine(reader, 10)
	if err != errLineTooLong {
		t.Fatalf("long line error = %v, want %v", err, errLineTooLong)
	}
}

func TestConfigRejectsDuplicateEngineIDs(t *testing.T) {
	config := DefaultConfig()
	config.Engines = []EngineConfig{
		{ID: "same", Name: "One", Version: "1", Command: "/bin/true"},
		{ID: "same", Name: "Two", Version: "2", Command: "/bin/true"},
	}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate engine id") {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestNewServerRejectsInvalidClientsFile(t *testing.T) {
	config := DefaultConfig()
	config.Auth = &AuthConfig{Enabled: true, ClientsFile: filepath.Join(t.TempDir(), "absent.config")}
	if _, err := NewServer(config, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("missing clients file accepted")
	}

	empty := filepath.Join(t.TempDir(), "clients.config")
	if err := os.WriteFile(empty, []byte("# nothing here\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config.Auth.ClientsFile = empty
	if _, err := NewServer(config, log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("clients file without entries accepted")
	}
}

func TestServerRequiresAuthentication(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	accessKey := "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	clientsPath := filepath.Join(t.TempDir(), "clients.config")
	if err := os.WriteFile(clientsPath, []byte("tester "+accessKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	config.Auth = &AuthConfig{Enabled: true, ClientsFile: clientsPath}
	server, err := NewServer(config, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()

	connection, err := listener.Dial()
	if err != nil {
		t.Fatal(err)
	}
	client := &testClient{connection: connection, reader: bufio.NewReader(connection)}
	hello := client.readResponse(t)
	if hello.Type != "hello" || hello.Protocol != ProtocolName {
		t.Fatalf("hello = %+v", hello)
	}
	hasAccessKeysFeature := false
	for _, feature := range hello.Features {
		if feature == "access_keys" {
			hasAccessKeysFeature = true
		}
	}
	if !hasAccessKeysFeature {
		t.Fatalf("hello features = %v, want access_keys", hello.Features)
	}

	// Requests other than authenticate are refused before authentication.
	client.send(Request{Type: "list_engines", RequestID: "early"})
	if response := client.readResponse(t); response.Type != "error" || response.Code != "authentication_required" {
		t.Fatalf("pre-authentication response = %+v", response)
	}

	// An invalid key is answered once and the connection is closed.
	client.send(Request{Type: "authenticate", RequestID: "bad", AccessKey: "3d813cbb-47fb-42ba-91df-831e1593ac29"})
	if response := client.readResponse(t); response.Type != "error" || response.Code != "invalid_access_key" || response.RequestID != "bad" {
		t.Fatalf("failed authentication response = %+v", response)
	}
	_ = connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.reader.ReadByte(); err == nil {
		t.Fatal("connection stayed open after a failed authentication")
	} else {
		_ = connection.Close()
	}

	// A valid key opens the normal session.
	connection, err = listener.Dial()
	if err != nil {
		t.Fatal(err)
	}
	client = &testClient{connection: connection, reader: bufio.NewReader(connection)}
	if hello := client.readResponse(t); hello.Type != "hello" {
		t.Fatalf("hello = %+v", hello)
	}
	client.send(Request{Type: "authenticate", RequestID: "ok", AccessKey: strings.ToUpper(accessKey)})
	if response := client.readResponse(t); response.Type != "authenticated" || response.RequestID != "ok" {
		t.Fatalf("authentication response = %+v", response)
	}

	// Re-authenticating an authenticated connection is refused.
	client.send(Request{Type: "authenticate", RequestID: "again", AccessKey: accessKey})
	if response := client.readResponse(t); response.Type != "error" || response.Code != "already_authenticated" {
		t.Fatalf("re-authentication response = %+v", response)
	}

	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	client.send(Request{Type: "uci", Command: "isready"})
	if !client.readUntilLine(t, "readyok") {
		t.Fatal("authenticated client did not receive readyok")
	}

	cancel()
	select {
	case err := <-serveErrors:
		if err != nil {
			t.Fatalf("server error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
	_ = client.connection.Close()
}

func TestServerRelaysIndependentEngineSessions(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxClients = 2
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
		{ID: "test-b", Name: "Test Engine B", Version: "2.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	server, err := NewServer(config, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()

	clients := make([]*testClient, 2)
	for index := range clients {
		connection, err := listener.Dial()
		if err != nil {
			t.Fatal(err)
		}
		clients[index] = &testClient{connection: connection, reader: bufio.NewReader(connection)}
		response := clients[index].readResponse(t)
		if response.Type != "hello" || response.Protocol != ProtocolName {
			t.Fatalf("hello = %+v", response)
		}
	}

	for _, client := range clients {
		client.send(Request{Type: "list_engines", RequestID: "list"})
		response := client.readResponse(t)
		if response.Type != "engines" || len(response.Engines) != 2 {
			t.Fatalf("engine list = %+v", response)
		}
	}

	clients[0].send(Request{Type: "select_engine", RequestID: "select-a", EngineID: "test-a"})
	if response := clients[0].readResponse(t); response.Type != "engine_selected" || response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	clients[1].send(Request{Type: "select_engine", RequestID: "select-b", EngineID: "test-b"})
	if response := clients[1].readResponse(t); response.Type != "engine_selected" || response.EngineID != "test-b" {
		t.Fatalf("select response = %+v", response)
	}

	clients[0].send(Request{Type: "uci", RequestID: "uci-a", Command: "uci"})
	clients[1].send(Request{Type: "uci", RequestID: "uci-b", Command: "uci"})
	if !clients[0].readUntilLine(t, "uciok") || !clients[1].readUntilLine(t, "uciok") {
		t.Fatal("did not receive uciok from both engine sessions")
	}

	clients[0].send(Request{Type: "uci", Command: "isready"})
	if !clients[0].readUntilLine(t, "readyok") {
		t.Fatal("client 0 did not receive readyok")
	}
	clients[1].send(Request{Type: "uci", Command: "isready"})
	if !clients[1].readUntilLine(t, "readyok") {
		t.Fatal("client 1 did not receive readyok")
	}
	clients[1].send(Request{Type: "uci", Command: "setoption name Multi PV value 2"})
	clients[1].send(Request{Type: "uci", Command: "position startpos moves e2e4 e7e5"})
	clients[1].send(Request{Type: "uci", Command: "go wtime 300000 btime 300000 depth 4"})
	if !clients[1].readUntilLine(t, "bestmove e2e4") {
		t.Fatal("client 1 did not receive a bestmove for a full go command")
	}

	clients[0].send(Request{Type: "select_engine", RequestID: "switch", EngineID: "test-b"})
	if response := clients[0].readUntilType(t, "engine_selected"); response.EngineID != "test-b" {
		t.Fatalf("switch response = %+v", response)
	}
	clients[0].send(Request{Type: "stop_engine", RequestID: "stop"})
	if response := clients[0].readUntilType(t, "engine_stopped"); response.EngineID != "test-b" {
		t.Fatalf("stop response = %+v", response)
	}

	// Stopping client 0 must not stop client 1's process.
	clients[1].send(Request{Type: "uci", Command: "isready"})
	if !clients[1].readUntilLine(t, "readyok") {
		t.Fatal("client 1 engine stopped with client 0")
	}
	cancel()
	select {
	case err := <-serveErrors:
		if err != nil {
			t.Fatalf("server error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
	for _, client := range clients {
		_ = client.connection.Close()
	}
}

type testClient struct {
	connection net.Conn
	reader     *bufio.Reader
}

// pipeListener keeps the integration test independent of the sandbox's
// network policy while exercising the same net.Listener server path.
type pipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		connections: make(chan net.Conn),
		closed:      make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case connection := <-l.connections:
		return connection, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr("pipe") }

func (l *pipeListener) Dial() (net.Conn, error) {
	serverConnection, clientConnection := net.Pipe()
	select {
	case l.connections <- serverConnection:
		return clientConnection, nil
	case <-l.closed:
		_ = serverConnection.Close()
		_ = clientConnection.Close()
		return nil, net.ErrClosed
	}
}

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

func (c *testClient) send(request Request) {
	data, err := json.Marshal(request)
	if err != nil {
		panic(err)
	}
	data = append(data, '\n')
	if _, err := c.connection.Write(data); err != nil {
		panic(err)
	}
}

func (c *testClient) sendRaw(data string) {
	if _, err := io.WriteString(c.connection, data); err != nil {
		panic(err)
	}
}

func (c *testClient) readResponse(t *testing.T) Response {
	t.Helper()
	_ = c.connection.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, err := c.reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("decode response %q: %v", data, err)
	}
	return response
}

func (c *testClient) readUntilLine(t *testing.T, wanted string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := c.readResponse(t)
		if response.Type == "uci_output" && response.Line == wanted {
			return true
		}
	}
	return false
}

func (c *testClient) readUntilType(t *testing.T, wanted string) Response {
	t.Helper()
	for {
		response := c.readResponse(t)
		if response.Type == wanted {
			return response
		}
	}
}

// readUntilTypeTimeout returns the first response of the wanted type, or false
// when the overall timeout expires. Unlike readUntilType it cannot hang the
// test suite when an event never arrives.
func (c *testClient) readUntilTypeTimeout(t *testing.T, wanted string, timeout time.Duration) (Response, bool) {
	t.Helper()
	_ = c.connection.SetReadDeadline(time.Now().Add(timeout))
	defer c.connection.SetReadDeadline(time.Time{})
	for {
		data, err := c.reader.ReadBytes('\n')
		if err != nil {
			return Response{}, false
		}
		var response Response
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatalf("decode response %q: %v", data, err)
		}
		if response.Type == wanted {
			return response, true
		}
	}
}

func startPipeServer(t *testing.T, config Config) (*pipeListener, context.CancelFunc) {
	t.Helper()
	server, err := NewServer(config, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	listener := newPipeListener()
	ctx, cancel := context.WithCancel(context.Background())
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-serveErrors:
			if err != nil {
				t.Errorf("server error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	})
	return listener, cancel
}

func dialPipeClient(t *testing.T, listener *pipeListener) *testClient {
	t.Helper()
	connection, err := listener.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	client := &testClient{connection: connection, reader: bufio.NewReader(connection)}
	if hello := client.readResponse(t); hello.Type != "hello" || hello.Protocol != ProtocolName {
		t.Fatalf("hello = %+v", hello)
	}
	return client
}

func TestValidateUCICommandRejectsEmpty(t *testing.T) {
	for _, command := range []string{"", "   ", "\t \t"} {
		if err := validateUCICommand(command); err == nil {
			t.Fatalf("validateUCICommand(%q) accepted", command)
		}
	}
	if err := validateUCICommand("uci"); err != nil {
		t.Fatalf("validateUCICommand(uci) = %v", err)
	}
}

func TestConfigValidatesMinSelectInterval(t *testing.T) {
	config := DefaultConfig()
	config.MinSelectIntervalMS = -1
	if err := config.Validate(); err == nil {
		t.Fatal("negative min_select_interval_ms accepted")
	}
	config.MinSelectIntervalMS = 60001
	if err := config.Validate(); err == nil {
		t.Fatal("oversized min_select_interval_ms accepted")
	}
	config.MinSelectIntervalMS = 0
	if err := config.Validate(); err != nil {
		t.Fatalf("disabled min_select_interval_ms rejected: %v", err)
	}
}

func TestServerRejectsEmptyUCICommand(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	for _, command := range []string{"", "   "} {
		client.send(Request{Type: "uci", RequestID: "empty", Command: command})
		if response := client.readUntilType(t, "error"); response.Code != "invalid_uci_command" {
			t.Fatalf("empty command response = %+v", response)
		}
	}
	// The session must still be usable after rejected commands.
	client.send(Request{Type: "uci", Command: "isready"})
	if !client.readUntilLine(t, "readyok") {
		t.Fatal("engine did not answer after rejected empty commands")
	}
}

func TestSelectSameEngineIsNoopAndCooldown(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 5000
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
		{ID: "test-b", Name: "Test Engine B", Version: "2.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.send(Request{Type: "select_engine", RequestID: "first", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	// Switching immediately must be refused without churning processes.
	client.send(Request{Type: "select_engine", RequestID: "fast", EngineID: "test-b"})
	if response := client.readUntilType(t, "error"); response.Code != "select_too_frequent" {
		t.Fatalf("fast switch response = %+v", response)
	}
	// Re-selecting the attached engine bypasses the cooldown and keeps the
	// running process: the search state must survive.
	client.send(Request{Type: "select_engine", RequestID: "same", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("same-engine response = %+v", response)
	}
	client.send(Request{Type: "uci", Command: "isready"})
	if !client.readUntilLine(t, "readyok") {
		t.Fatal("attached engine did not survive same-engine select")
	}
}

func TestFailedSelectArmsCooldown(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 5000
	config.Engines = []EngineConfig{
		{ID: "missing", Name: "Missing Engine", Version: "0", Command: filepath.Join(t.TempDir(), "no-such-engine")},
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	// A failed start still forked, so it arms the cooldown like a success.
	client.send(Request{Type: "select_engine", RequestID: "bad", EngineID: "missing"})
	if response := client.readUntilType(t, "error"); response.Code != "engine_start_failed" {
		t.Fatalf("failed start response = %+v", response)
	}
	client.send(Request{Type: "select_engine", RequestID: "fast", EngineID: "test-a"})
	if response := client.readUntilType(t, "error"); response.Code != "select_too_frequent" {
		t.Fatalf("post-failure switch response = %+v", response)
	}
	// Unknown engine ids are cheap (no fork) and never arm the cooldown on
	// their own: covered implicitly since the failure above, not the unknown
	// id, is what throttles.
}

func TestServerReportsEngineExited(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER_EXIT", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "crash", Name: "Crash Engine", Version: "0", Command: os.Args[0], Args: []string{"-test.run=TestUCIExitHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "crash"})
	// A process that dies immediately may report engine_exited before the
	// select acknowledgement reaches the queue: accept either order.
	selected, exited := false, false
	_ = client.connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer client.connection.SetReadDeadline(time.Time{})
	for !(selected && exited) {
		data, err := client.reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("waiting for engine_selected/engine_exited: %v", err)
		}
		var response Response
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatalf("decode response %q: %v", data, err)
		}
		switch response.Type {
		case "engine_selected":
			if response.EngineID != "crash" {
				t.Fatalf("select response = %+v", response)
			}
			selected = true
		case "engine_exited":
			if response.EngineID != "crash" {
				t.Fatalf("engine_exited = %+v", response)
			}
			exited = true
		}
	}
	// The dead process must be detached: further commands report
	// no_engine_selected instead of writing to a broken pipe.
	client.send(Request{Type: "uci", RequestID: "after", Command: "isready"})
	if response := client.readUntilType(t, "error"); response.Code != "no_engine_selected" {
		t.Fatalf("post-crash response = %+v", response)
	}
}

func TestServerRejectsMalformedFrames(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 1024
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.sendRaw("this is not json\n")
	if response := client.readResponse(t); response.Type != "error" || response.Code != "invalid_json" {
		t.Fatalf("invalid json response = %+v", response)
	}
	client.send(Request{Type: "bogus", RequestID: "unknown"})
	if response := client.readResponse(t); response.Type != "error" || response.Code != "unknown_request_type" {
		t.Fatalf("unknown type response = %+v", response)
	}
	client.send(Request{Type: "select_engine", RequestID: "missing", EngineID: "nope"})
	if response := client.readResponse(t); response.Type != "error" || response.Code != "unknown_engine" {
		t.Fatalf("unknown engine response = %+v", response)
	}
	client.send(Request{Type: "uci", RequestID: "early", Command: "isready"})
	if response := client.readResponse(t); response.Type != "error" || response.Code != "no_engine_selected" {
		t.Fatalf("no-engine response = %+v", response)
	}
	client.sendRaw(strings.Repeat("x", 2048) + "\n")
	if response := client.readResponse(t); response.Type != "error" || response.Code != "line_too_long" {
		t.Fatalf("long line response = %+v", response)
	}
	// Every rejection above keeps the connection: the session still works.
	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	client.send(Request{Type: "uci", Command: "isready"})
	if !client.readUntilLine(t, "readyok") {
		t.Fatal("engine did not answer after malformed frames")
	}
}

func TestServerBusy(t *testing.T) {
	config := DefaultConfig()
	config.MaxClients = 1
	config.MinSelectIntervalMS = 0
	listener, _ := startPipeServer(t, config)

	holder, err := listener.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if hello := readOneResponse(t, holder, bufio.NewReader(holder)); hello.Type != "hello" {
		t.Fatalf("hello = %+v", hello)
	}

	overflow, err := listener.Dial()
	if err != nil {
		t.Fatal(err)
	}
	defer overflow.Close()
	response := readOneResponse(t, overflow, bufio.NewReader(overflow))
	if response.Type != "error" || response.Code != "server_busy" {
		t.Fatalf("busy response = %+v", response)
	}
	_ = overflow.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := overflow.Read(make([]byte, 1)); err == nil {
		t.Fatal("overflow connection stayed open after server_busy")
	}
}

// readOneResponse reads a single frame with a bounded deadline. Unlike
// readResponse it does not fail the test on timeout: callers assert.
func readOneResponse(t *testing.T, connection net.Conn, reader *bufio.Reader) Response {
	t.Helper()
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer connection.SetReadDeadline(time.Time{})
	data, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var response Response
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatalf("decode response %q: %v", data, err)
	}
	return response
}

func TestQuitDetachesEngine(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	// The UCI quit command is relayed and releases the engine from the
	// connection without any engine_exited event (expected shutdown).
	client.send(Request{Type: "uci", RequestID: "quit", Command: "quit"})
	client.send(Request{Type: "uci", RequestID: "after", Command: "isready"})
	if response := client.readUntilType(t, "error"); response.Code != "no_engine_selected" || response.RequestID != "after" {
		t.Fatalf("post-quit response = %+v", response)
	}
	_ = client.connection.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if data, err := client.reader.ReadBytes('\n'); err == nil {
		t.Fatalf("unexpected frame after quit: %q", data)
	}
	_ = client.connection.SetReadDeadline(time.Time{})
}

func TestEngineStderrIsDrained(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0], Args: []string{"-test.run=TestUCIHelperProcess"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "test-a"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "test-a" {
		t.Fatalf("select response = %+v", response)
	}
	// "noisy" makes the helper write to stderr; the stdout stream must stay
	// intact and stderr must never reach the client.
	client.send(Request{Type: "uci", Command: "noisy"})
	if !client.readUntilLine(t, "readyok") {
		t.Fatal("stdout stream broke while the engine wrote to stderr")
	}
}

func TestServerTLS(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)

	config := DefaultConfig()
	config.MinSelectIntervalMS = 0
	config.Engines = []EngineConfig{
		{ID: "test-a", Name: "Test Engine A", Version: "1.0", Command: os.Args[0]},
	}
	config.TLS = &TLSConfig{CertFile: certFile, KeyFile: keyFile}
	server, err := NewServer(config, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	rawListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sandbox has no loopback TCP: %v", err)
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	listener := tls.NewListener(rawListener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS13,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(ctx, listener) }()
	defer func() {
		cancel()
		select {
		case err := <-serveErrors:
			if err != nil {
				t.Errorf("server error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server did not stop")
		}
	}()

	connection, err := tls.Dial("tcp", rawListener.Addr().String(), &tls.Config{ //nolint:gosec // test-only, self-signed cert
		InsecureSkipVerify: true, //nolint:gosec // test-only, self-signed cert
		MinVersion:         tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	reader := bufio.NewReader(connection)
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read hello: %v", err)
	}
	var hello Response
	if err := json.Unmarshal(data, &hello); err != nil || hello.Type != "hello" || hello.Protocol != ProtocolName {
		t.Fatalf("hello = %q, %v", data, err)
	}
	frame, _ := json.Marshal(Request{Type: "list_engines", RequestID: "tls"})
	_ = connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := connection.Write(append(frame, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, err = reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read engines: %v", err)
	}
	var engines Response
	if err := json.Unmarshal(data, &engines); err != nil || engines.Type != "engines" || len(engines.Engines) != 1 {
		t.Fatalf("engines = %q, %v", data, err)
	}
}

func writeSelfSignedCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "chessgateway-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	certOut, err := os.Create(certFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	_ = certOut.Close()
	keyOut, err := os.OpenFile(keyFile, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}); err != nil {
		t.Fatal(err)
	}
	_ = keyOut.Close()
	return certFile, keyFile
}

func TestShutdownKillsProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process-group assertions use /proc")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	const marker = "sleep 29731"
	config := DefaultConfig()
	config.ShutdownTimeoutMS = 300
	config.MinSelectIntervalMS = 0
	// The backgrounded sleep is a child of sh: killing only sh would orphan
	// it. The gateway must kill the whole process group.
	config.Engines = []EngineConfig{
		{ID: "group", Name: "Group Engine", Version: "1", Command: "sh", Args: []string{"-c", marker + " & wait"}},
	}
	listener, _ := startPipeServer(t, config)
	client := dialPipeClient(t, listener)

	client.send(Request{Type: "select_engine", RequestID: "select", EngineID: "group"})
	if response := client.readUntilType(t, "engine_selected"); response.EngineID != "group" {
		t.Fatalf("select response = %+v", response)
	}
	waitForCmdline(t, marker, true)
	client.send(Request{Type: "stop_engine", RequestID: "stop"})
	if response, ok := client.readUntilTypeTimeout(t, "engine_stopped", 10*time.Second); !ok || response.EngineID != "group" {
		t.Fatalf("stop response = %+v, %v", response, ok)
	}
	waitForCmdline(t, marker, false)
}

// waitForCmdline polls /proc for a command line containing marker until
// present (want=true) or gone (want=false).
func waitForCmdline(t *testing.T, marker string, want bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		found := false
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
			if err != nil {
				continue
			}
			if strings.Contains(string(data), marker) {
				found = true
				break
			}
		}
		if found == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("cmdline containing %q present=%v, want %v", marker, !want, want)
}
