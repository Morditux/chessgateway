package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var errNoEngine = errors.New("no engine is selected")

type engineProcess struct {
	config       EngineConfig
	maxLineBytes int
	logger       *log.Logger
	onOutput     func(*engineProcess, string)

	writeMu sync.Mutex // Serializes pipe writes without blocking lifecycle state.
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	closing bool
	done    chan struct{}
	stopping chan struct{}
}

func startEngine(config EngineConfig, maxLineBytes int, logger *log.Logger, onOutput func(*engineProcess, string), onExit func(*engineProcess)) (*engineProcess, error) {
	command := exec.Command(config.Command, config.Args...)
	configureSysProcAttr(command)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		_ = stdout.Close()
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, err
	}

	process := &engineProcess{
		config:       config,
		maxLineBytes: maxLineBytes,
		logger:       logger,
		onOutput:     onOutput,
		cmd:          command,
		stdin:        stdin,
		done:         make(chan struct{}),
		stopping:     make(chan struct{}),
	}

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		process.readStdout(stdout)
	}()
	go func() {
		defer readers.Done()
		process.readStderr(stderr)
	}()
	go func() {
		err := command.Wait()
		readers.Wait()
		if err != nil && logger != nil {
			logger.Printf("engine %q exited: %v", config.ID, err)
		}
		process.mu.Lock()
		expected := process.closing
		process.mu.Unlock()
		close(process.done)
		// An engine that dies on its own (crash, OOM, external kill) must
		// wake the client: otherwise a search hangs forever waiting for a
		// bestmove that will never come. Expected shutdowns (stop, select,
		// quit, disconnect) already answer synchronously and stay silent.
		if !expected && onExit != nil {
			onExit(process)
		}
	}()

	return process, nil
}

func (p *engineProcess) readStdout(reader io.ReadCloser) {
	defer reader.Close()
	buffered := bufio.NewReader(reader)
	for {
		line, err := readLine(buffered, p.maxLineBytes)
		if errors.Is(err, errLineTooLong) {
			if p.logger != nil {
				p.logger.Printf("engine %q output line exceeded configured limit and was discarded", p.config.ID)
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && p.logger != nil {
				p.logger.Printf("engine %q stdout: %v", p.config.ID, err)
			}
			return
		}
		if p.onOutput != nil {
			p.onOutput(p, line)
		}
	}
}

func (p *engineProcess) readStderr(reader io.ReadCloser) {
	defer reader.Close()
	buffered := bufio.NewReader(reader)
	for {
		line, err := readLine(buffered, p.maxLineBytes)
		if errors.Is(err, errLineTooLong) {
			if p.logger != nil {
				p.logger.Printf("engine %q stderr line exceeded configured limit and was discarded", p.config.ID)
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && p.logger != nil {
				p.logger.Printf("engine %q stderr: %v", p.config.ID, err)
			}
			return
		}
		// Stderr is deliberately not sent to clients: the UCI stream is stdout.
		if p.logger != nil && line != "" {
			// %q keeps control characters from becoming log-injection sequences.
			p.logger.Printf("engine %q stderr: %q", p.config.ID, line)
		}
	}
}

// engineWriteTimeout bounds an unresponsive engine even while the client
// connection remains open. Network deadlines cannot interrupt a pipe write.
const engineWriteTimeout = 30 * time.Second

func (p *engineProcess) send(command string) error {
	return p.sendWithTimeout(command, engineWriteTimeout)
}

func (p *engineProcess) sendWithTimeout(command string, timeout time.Duration) error {
	result := make(chan error, 1)
	go func() {
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		p.mu.Lock()
		closing := p.closing
		p.mu.Unlock()
		if closing {
			result <- errors.New("engine is stopping")
			return
		}
		_, err := io.WriteString(p.stdin, command+"\n")
		result <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-p.stopping:
		return errors.New("engine is stopping")
	case <-timer.C:
		// An expired write must release its writer, not leave a goroutine
		// and an engine behind. Zero grace forces immediate termination.
		p.shutdown(0)
		return errors.New("engine command write timed out")
	}
}

func (p *engineProcess) shutdown(timeout time.Duration) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		<-p.done
		return
	}
	p.closing = true
	// Release output callbacks before waiting for the process/readers.
	close(p.stopping)
	p.mu.Unlock()

	// Arm the timeout before attempting graceful writes or acquiring their
	// lock: either can block when the engine stops reading stdin.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		p.writeMu.Lock()
		defer p.writeMu.Unlock()
		_, _ = io.WriteString(p.stdin, "stop\nquit\n")
		_ = p.stdin.Close()
	}()
	select {
	case <-p.done:
	case <-timer.C:
		_ = killProcess(p.cmd)
	}
	// Close also interrupts outstanding pipe writes; no lifecycle mutex is
	// held by those writers. Join cleanup before returning.
	_ = p.stdin.Close()
	<-writerDone
	<-p.done
}

type engineSession struct {
	logger          *log.Logger
	maxLineBytes    int
	shutdownTimeout time.Duration
	onOutput        func(engineID, line string, stopping <-chan struct{})
	onExit          func(engineID string)

	mu      sync.Mutex
	current *engineProcess
	engine  *EngineConfig
}

func newEngineSession(config Config, logger *log.Logger, onOutput func(engineID, line string, stopping <-chan struct{}), onExit func(engineID string)) *engineSession {
	return &engineSession{
		logger:          logger,
		maxLineBytes:    config.MaxLineBytes,
		shutdownTimeout: config.shutdownTimeout(),
		onOutput:        onOutput,
		onExit:          onExit,
	}
}

func (s *engineSession) selectEngine(config EngineConfig) error {
	s.stop()

	var process *engineProcess
	var err error
	// The callbacks receive the started process as a parameter instead of
	// closing over the local below: startEngine spawns its readers before it
	// returns, so a closure over the not-yet-assigned variable would race.
	process, err = startEngine(config, s.maxLineBytes, s.logger, func(current *engineProcess, line string) {
		s.mu.Lock()
		isCurrent := s.current != nil && s.current == current
		s.mu.Unlock()
		if isCurrent && s.onOutput != nil {
			s.onOutput(config.ID, line, current.stopping)
		}
	}, func(current *engineProcess) {
		s.handleUnexpectedExit(current, config.ID)
	})
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("start engine %q (%s %s): %v", config.ID, config.Name, config.Version, err)
		}
		return err
	}

	s.mu.Lock()
	s.current = process
	s.engine = &config
	s.mu.Unlock()
	return nil
}

func (s *engineSession) send(command string) error {
	s.mu.Lock()
	process := s.current
	s.mu.Unlock()
	if process == nil {
		return errNoEngine
	}
	if err := process.send(command); err != nil {
		select {
		case <-process.stopping:
			s.stopProcess(process)
		default:
		}
		return err
	}
	if strings.TrimSpace(command) == "quit" {
		s.stopProcess(process)
	}
	return nil
}

func (s *engineSession) currentID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine == nil {
		return ""
	}
	return s.engine.ID
}

// handleUnexpectedExit clears a process that died on its own so later
// commands report no_engine_selected instead of writing to a dead pipe,
// then notifies the client. The callback runs outside the session lock.
func (s *engineSession) handleUnexpectedExit(process *engineProcess, engineID string) {
	s.mu.Lock()
	if s.current != process {
		s.mu.Unlock()
		return
	}
	s.current = nil
	s.engine = nil
	onExit := s.onExit
	s.mu.Unlock()
	if onExit != nil {
		onExit(engineID)
	}
}

func (s *engineSession) stop() string {
	s.mu.Lock()
	process := s.current
	engineID := ""
	if s.engine != nil {
		engineID = s.engine.ID
	}
	s.mu.Unlock()
	if process != nil {
		process.shutdown(s.shutdownTimeout)
	}
	s.mu.Lock()
	if process != nil && s.current == process {
		s.current = nil
		s.engine = nil
	}
	s.mu.Unlock()
	return engineID
}

func (s *engineSession) stopProcess(process *engineProcess) {
	s.mu.Lock()
	if s.current != process {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	process.shutdown(s.shutdownTimeout)
	s.mu.Lock()
	if s.current == process {
		s.current = nil
		s.engine = nil
	}
	s.mu.Unlock()
}
