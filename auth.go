package gateway

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// ClientAccess is one entry of the clients access-key file: an administrative
// label and the unique UUID key the client must present.
type ClientAccess struct {
	Name string
	Key  string
}

// maxClientsEntries bounds the number of credentials so that the scan
// performed by authenticate stays cheap and files stay small.
const maxClientsEntries = 4096

// accessKeyStore holds the configured client access keys, keyed by their
// canonical lowercase form.
type accessKeyStore struct {
	keys map[string]string
}

func newAccessKeyStore(entries []ClientAccess) *accessKeyStore {
	keys := make(map[string]string, len(entries))
	for _, entry := range entries {
		keys[entry.Key] = entry.Name
	}
	return &accessKeyStore{keys: keys}
}

// authenticate reports whether key is valid and returns the associated client
// name. Keys are canonicalized to lowercase and looked up directly: UUIDs
// carry ~122 bits of entropy, so map-lookup timing reveals nothing
// exploitable, unlike a byte-prefix comparison loop.
func (s *accessKeyStore) authenticate(key string) (string, bool) {
	if !isValidUUID(key) {
		return "", false
	}
	name, ok := s.keys[strings.ToLower(key)]
	return name, ok
}

// warnClientsFilePerms reminds operators that the access-key file holds bearer
// secrets: any group/other access bit defeats file-based protection. It only
// warns so existing deployments keep starting.
func warnClientsFilePerms(path string, logger *log.Logger) {
	if logger == nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		logger.Printf("clients file %q is accessible beyond its owner (mode %04o); restrict it, e.g. chmod 600", path, perm)
	}
}

// loadClientsFile parses the access-key file. Each non-empty line that is not
// a comment contains a client name and its UUID key separated by whitespace.
func loadClientsFile(path string) ([]ClientAccess, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open clients file: %w", err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil {
		return nil, fmt.Errorf("stat clients file: %w", err)
	} else if info.Size() > maxConfigBytes {
		return nil, errors.New("clients file exceeds the maximum size")
	}

	var entries []ClientAccess
	seenNames := make(map[string]struct{})
	seenKeys := make(map[string]struct{})
	scanner := bufio.NewScanner(io.LimitReader(file, maxConfigBytes))
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<10)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("clients file line %d: expected a name and a UUID key", lineNumber)
		}
		name, key := fields[0], fields[1]
		if err := validateClientName(name); err != nil {
			return nil, fmt.Errorf("clients file line %d: %w", lineNumber, err)
		}
		if !isValidUUID(key) {
			return nil, fmt.Errorf("clients file line %d: key is not a valid UUID", lineNumber)
		}
		key = strings.ToLower(key)
		if _, exists := seenNames[name]; exists {
			return nil, fmt.Errorf("clients file line %d: duplicate client name %q", lineNumber, name)
		}
		if _, exists := seenKeys[key]; exists {
			return nil, fmt.Errorf("clients file line %d: duplicate access key", lineNumber)
		}
		if len(entries)+1 > maxClientsEntries {
			return nil, fmt.Errorf("clients file exceeds the maximum of %d entries", maxClientsEntries)
		}
		seenNames[name] = struct{}{}
		seenKeys[key] = struct{}{}
		entries = append(entries, ClientAccess{Name: name, Key: key})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read clients file: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("clients file does not contain any client")
	}
	return entries, nil
}

func validateClientName(name string) error {
	if name == "" || len(name) > 64 {
		return errors.New("name must be between 1 and 64 characters")
	}
	for _, r := range name {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return errors.New("name may contain only ASCII letters, digits, '.', '_' and '-'")
		}
	}
	return nil
}

// isValidUUID reports whether value is a canonical textual UUID
// (8-4-4-4-12 hexadecimal digits), case-insensitively.
func isValidUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			switch {
			case character >= '0' && character <= '9':
			case character >= 'a' && character <= 'f':
			case character >= 'A' && character <= 'F':
			default:
				return false
			}
		}
	}
	return true
}
