package gateway

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeClientsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "clients.config")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadClientsFile(t *testing.T) {
	path := writeClientsFile(t,
		"# comment\r\n"+
			"\r\n"+
			"alice F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6\n"+
			"bob\t3d813cbb-47fb-42ba-91df-831e1593ac29\n")
	entries, err := loadClientsFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Name != "alice" || entries[0].Key != "f81d4fae-7dec-11d0-a765-00a0c91e6bf6" {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[1].Name != "bob" {
		t.Fatalf("second entry = %+v", entries[1])
	}
}

func TestLoadClientsFileRejects(t *testing.T) {
	key := "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	other := "3d813cbb-47fb-42ba-91df-831e1593ac29"
	cases := map[string]string{
		"no entries":     "# only a comment\n",
		"missing key":    "alice\n",
		"extra field":    "alice " + key + " extra\n",
		"name separator": "al ice " + key + "\n",
		"forbidden name": "al!ice " + key + "\n",
		"unicode name":   "clé " + key + "\n",
		"invalid uuid":   "alice not-a-uuid\n",
		"duplicate name": "alice " + key + "\nalice " + other + "\n",
		"duplicate key":  "alice " + key + "\nbob F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6\n",
		"name too long":  strings.Repeat("a", 65) + " " + key + "\n",
		"non-hex uuid":   "alice f81d4fae-7dec-11g0-a765-00a0c91e6bf6\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeClientsFile(t, content)
			if _, err := loadClientsFile(path); err == nil {
				t.Fatalf("loadClientsFile accepted %q", content)
			}
		})
	}
}

func TestLoadClientsFileMissing(t *testing.T) {
	if _, err := loadClientsFile(filepath.Join(t.TempDir(), "absent.config")); err == nil {
		t.Fatal("missing clients file accepted")
	}
}

func TestIsValidUUID(t *testing.T) {
	valid := []string{
		"f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
		"F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6",
		"00000000-0000-0000-0000-000000000000",
	}
	invalid := []string{
		"",
		"f81d4fae7dec11d0a76500a0c91e6bf6",
		"f81d4fae_7dec-11d0-a765-00a0c91e6bf6",
		"f81d4fae-7dec-11d0-a765-00a0c91e6bf",
		"f81d4fae-7dec-11d0-a765-00a0c91e6bf66",
		"g81d4fae-7dec-11d0-a765-00a0c91e6bf6",
		"{f81d4fae-7dec-11d0-a765-00a0c91e6bf6}",
	}
	for _, value := range valid {
		if !isValidUUID(value) {
			t.Fatalf("isValidUUID(%q) = false", value)
		}
	}
	for _, value := range invalid {
		if isValidUUID(value) {
			t.Fatalf("isValidUUID(%q) = true", value)
		}
	}
}

func TestAccessKeyStore(t *testing.T) {
	store := newAccessKeyStore([]ClientAccess{
		{Name: "alice", Key: "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"},
	})
	name, ok := store.authenticate("F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6")
	if !ok || name != "alice" {
		t.Fatalf("authenticate uppercase = %q, %v", name, ok)
	}
	if _, ok := store.authenticate("3d813cbb-47fb-42ba-91df-831e1593ac29"); ok {
		t.Fatal("unknown key accepted")
	}
	if _, ok := store.authenticate("not-a-uuid"); ok {
		t.Fatal("malformed key accepted")
	}
	if _, ok := store.authenticate(""); ok {
		t.Fatal("empty key accepted")
	}
}

func TestNewServerWarnsOnPermissiveClientsFile(t *testing.T) {
	path := writeClientsFile(t, "alice f81d4fae-7dec-11d0-a765-00a0c91e6bf6\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0o077 == 0 {
		t.Skip("umask strips group/other bits, warning cannot trigger here")
	}
	var logs bytes.Buffer
	config := DefaultConfig()
	config.Auth = &AuthConfig{Enabled: true, ClientsFile: path}
	if _, err := NewServer(config, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("permissive clients file rejected: %v", err)
	}
	if !strings.Contains(logs.String(), "chmod 600") {
		t.Fatalf("missing permissions warning, logs = %q", logs.String())
	}

	restricted := writeClientsFile(t, "alice f81d4fae-7dec-11d0-a765-00a0c91e6bf6\n")
	logs.Reset()
	config.Auth.ClientsFile = restricted
	if _, err := NewServer(config, log.New(&logs, "", 0)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "chmod 600") {
		t.Fatalf("unexpected permissions warning, logs = %q", logs.String())
	}
}

func TestConfigValidatesAuth(t *testing.T) {
	config := DefaultConfig()
	config.Auth = &AuthConfig{Enabled: true}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "clients_file") {
		t.Fatalf("Validate() error = %v", err)
	}
	config.Auth.ClientsFile = "clie\x00nts.config"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("Validate() error = %v", err)
	}
	config.Auth = &AuthConfig{Enabled: false}
	if err := config.Validate(); err != nil {
		t.Fatalf("disabled auth rejected: %v", err)
	}
	config.Auth = &AuthConfig{Enabled: true, ClientsFile: "clients.config"}
	if err := config.Validate(); err != nil {
		t.Fatalf("valid auth rejected: %v", err)
	}
}
