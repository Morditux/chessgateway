package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	// ProtocolName is the protocol identifier sent in the initial hello message.
	ProtocolName = "chessgateway/1"

	defaultMaxLineBytes = 1 << 20 // 1 MiB
)

var errLineTooLong = errors.New("protocol line exceeds configured limit")

// Request is one JSON Lines request sent by a client. UCI commands are carried
// verbatim in Command, except for the transport newline added by the server.
type Request struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	EngineID  string `json:"engine_id,omitempty"`
	Command   string `json:"command,omitempty"`
	AccessKey string `json:"access_key,omitempty"`
}

// EngineInfo contains the public metadata of a configured engine. The command
// path and arguments are intentionally not exposed to clients.
type EngineInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Response is the common JSON Lines response/event envelope.
type Response struct {
	Type      string       `json:"type"`
	Protocol  string       `json:"protocol,omitempty"`
	RequestID string       `json:"request_id,omitempty"`
	EngineID  string       `json:"engine_id,omitempty"`
	Line      string       `json:"line,omitempty"`
	Code      string       `json:"code,omitempty"`
	Message   string       `json:"message,omitempty"`
	Features  []string     `json:"features,omitempty"`
	Engines   []EngineInfo `json:"engines,omitempty"`
	Engine    *EngineInfo  `json:"engine,omitempty"`
}

func readLine(reader *bufio.Reader, maxBytes int) (string, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxLineBytes
	}

	var line []byte
	tooLong := false
	for {
		fragment, isPrefix, err := reader.ReadLine()
		if err != nil {
			return "", err
		}

		if !tooLong {
			if len(line)+len(fragment) > maxBytes {
				tooLong = true
			} else {
				line = append(line, fragment...)
			}
		}

		if !isPrefix {
			if tooLong {
				return "", errLineTooLong
			}
			// ReadLine removes the LF but can leave a CR when a client uses
			// CRLF framing. JSON and UCI both treat it as transport framing.
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			return string(line), nil
		}
	}
}

func writeJSON(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = writer.Write(data)
	return err
}

func validateRequest(request Request) error {
	if request.Type == "" {
		return fmt.Errorf("missing type")
	}
	if len(request.RequestID) > 128 {
		return fmt.Errorf("request_id is too long")
	}
	if containsControlSeparator(request.RequestID) {
		return fmt.Errorf("request_id contains a line separator")
	}
	return nil
}

func containsControlSeparator(value string) bool {
	for _, r := range value {
		if r == '\r' || r == '\n' || r == 0 {
			return true
		}
	}
	return false
}

func validateUCICommand(command string) error {
	if containsControlSeparator(command) {
		return fmt.Errorf("command contains a line separator or NUL")
	}
	return nil
}
