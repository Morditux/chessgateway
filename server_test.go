package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

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

func TestServerRelaysIndependentEngineSessions(t *testing.T) {
	t.Setenv("CHESSGATEWAY_HELPER", "1")

	config := DefaultConfig()
	config.MaxClients = 2
	config.MaxLineBytes = 64 << 10
	config.ShutdownTimeoutMS = 500
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
