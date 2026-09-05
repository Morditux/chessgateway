package gateway

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

// This subprocess never reads stdin. In flood mode it also fills the output
// queue, reproducing backpressure independently of any installed UCI engine.
func TestBlockedEngineHelper(t *testing.T) {
	mode := os.Getenv("CHESSGATEWAY_BLOCKED_HELPER")
	if mode == "" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	if mode == "flood" {
		for {
			if _, err := fmt.Fprintln(os.Stdout, "info string output"); err != nil {
				break
			}
		}
	}
	time.Sleep(time.Hour)
	os.Exit(0)
}

func blockedTestEngine(t *testing.T, mode string, output func(*engineProcess, string)) *engineProcess {
	t.Helper()
	t.Setenv("CHESSGATEWAY_BLOCKED_HELPER", mode)
	ready := make(chan struct{})
	p, err := startEngine(EngineConfig{
		ID: "blocked", Name: "Blocked", Version: "test",
		Command: os.Args[0], Args: []string{"-test.run=^TestBlockedEngineHelper$"},
	}, 1024, log.New(io.Discard, "", 0), func(p *engineProcess, line string) {
		if line == "ready" {
			close(ready)
			return
		}
		if output != nil {
			output(p, line)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = killProcess(p.cmd)
		_ = p.stdin.Close()
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not start")
	}
	return p
}

func awaitShutdown(t *testing.T, p *engineProcess) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		p.shutdown(50 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked beyond its grace period")
	}
}

func TestEngineWriteTimeoutReapsProcess(t *testing.T) {
	p := blockedTestEngine(t, "silent", nil)
	result := make(chan error, 1)
	go func() {
		result <- p.sendWithTimeout(strings.Repeat("x", 4<<20), 100*time.Millisecond)
	}()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("write error = %v, want timeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("engine write did not time out")
	}
	select {
	case <-p.done:
	default:
		t.Fatal("timed-out engine was not reaped")
	}
}

func TestShutdownInterruptsBlockedEngineWrite(t *testing.T) {
	p := blockedTestEngine(t, "silent", nil)
	result := make(chan error, 1)
	go func() { result <- p.send(strings.Repeat("x", 4<<20)) }()
	// The non-reading helper cannot consume this command. Confirm the send
	// remains pending before testing cancellation by concurrent shutdown.
	select {
	case err := <-result:
		t.Fatalf("write unexpectedly completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	awaitShutdown(t, p)
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("blocked write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not release sender")
	}
}

func TestShutdownReleasesFullClientOutputQueue(t *testing.T) {
	c := &client{sendQueue: make(chan Response, 1), done: make(chan struct{})}
	c.sendQueue <- Response{Type: "uci_output"}
	entered := make(chan struct{}, 1)
	p := blockedTestEngine(t, "flood", func(p *engineProcess, line string) {
		select {
		case entered <- struct{}{}:
		default:
		}
		c.engineOutput("blocked", line, p.stopping)
	})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("engine did not produce output")
	}
	// Keep the client alive and its queue full: stop_engine and engine
	// replacement must release output even without connection teardown.
	awaitShutdown(t, p)
}
