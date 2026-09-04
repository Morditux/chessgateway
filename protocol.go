package gateway

import (
	"bufio"
	"io"

	"github.com/Morditux/chessgateway/internal/protocol"
)

// The wire types live in internal/protocol, shared with gatewayclient so the
// two sides cannot drift. They are aliased here to preserve the gateway
// package API.
type (
	Request    = protocol.Request
	EngineInfo = protocol.EngineInfo
	Response   = protocol.Response
)

const (
	// ProtocolName is the protocol identifier sent in the initial hello message.
	ProtocolName = protocol.ProtocolName

	defaultMaxLineBytes = protocol.DefaultMaxLineBytes
)

var errLineTooLong = protocol.ErrLineTooLong

func readLine(reader *bufio.Reader, maxBytes int) (string, error) {
	return protocol.ReadLine(reader, maxBytes)
}

func writeJSON(writer io.Writer, value any) error {
	return protocol.WriteJSON(writer, value)
}

func validateRequest(request Request) error {
	return protocol.ValidateRequest(request)
}

func containsControlSeparator(value string) bool {
	return protocol.ContainsControlSeparator(value)
}

func validateUCICommand(command string) error {
	return protocol.ValidateUCICommand(command)
}
