package client

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"testing"
	"time"
)

// TestRunBridgesUCI drives Run against a fake gateway on an in-memory pipe:
// GUI stdin must reach the gateway as uci frames and gateway uci_output must
// reach GUI stdout.
func TestRunBridgesUCI(t *testing.T) {
	gatewayEnd, clientEnd := net.Pipe()
	defer gatewayEnd.Close()
	defer clientEnd.Close()

	previousDial := dialGatewayFunc
	dialGatewayFunc = func(Config, *log.Logger) (net.Conn, error) { return clientEnd, nil }
	defer func() { dialGatewayFunc = previousDial }()

	serverErrors := make(chan error, 1)
	go func() { serverErrors <- fakeGateway(t, gatewayEnd) }()

	stdout := &strings.Builder{}
	logger := log.New(io.Discard, "", 0)
	config := DefaultConfig()
	config.EngineID = "stockfish-17"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Run(ctx, config, strings.NewReader("isready\n"), stdout, logger); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if err := <-serverErrors; err != nil {
		t.Fatalf("fake gateway = %v", err)
	}
	if out := stdout.String(); out != "readyok\n" {
		t.Fatalf("stdout = %q, want %q", out, "readyok\n")
	}
}

func fakeGateway(t *testing.T, connection net.Conn) error {
	t.Helper()
	defer connection.Close()
	reader := bufio.NewReader(connection)
	write := func(value Response) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_ = connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, err = connection.Write(append(data, '\n'))
		return err
	}
	read := func() Request {
		t.Helper()
		_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("fake gateway read: %v", err)
		}
		var request Request
		if err := json.Unmarshal(line, &request); err != nil {
			t.Fatalf("fake gateway decode %q: %v", line, err)
		}
		return request
	}

	if err := write(Response{Type: "hello", Protocol: ProtocolName, Features: []string{"engine_list"}}); err != nil {
		return err
	}
	if selectRequest := read(); selectRequest.Type != "select_engine" || selectRequest.EngineID != "stockfish-17" {
		t.Fatalf("select = %+v", selectRequest)
	}
	if err := write(Response{Type: "engine_selected", RequestID: "select", EngineID: "stockfish-17"}); err != nil {
		return err
	}
	for {
		request := read()
		if request.Type != "uci" {
			t.Fatalf("uci frame = %+v", request)
		}
		switch request.Command {
		case "isready":
			if err := write(Response{Type: "uci_output", EngineID: "stockfish-17", Line: "readyok"}); err != nil {
				return err
			}
		case "quit":
			// GUI stdin hits EOF right after isready, so the client sends
			// quit itself: closing here ends the bridge cleanly.
			return nil
		default:
			t.Fatalf("unexpected command %q", request.Command)
		}
	}
}
