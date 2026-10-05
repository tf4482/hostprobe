package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const validConfig = `database:
  host: "db.example"
  port: 5432
  name: "monitoring"
  user: "monitoring"
  password: "secret"
settings:
  ping_concurrency: 16
  ping_timeout_seconds: 1
  ssh_concurrency: 8
  ssh_timeout_seconds: 2.5
`

func parseYAML(t *testing.T, text string) (Config, error) {
	t.Helper()
	var document any
	if err := yaml.Unmarshal([]byte(text), &document); err != nil {
		t.Fatal(err)
	}
	return parseConfig(document)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestExecutableConfigTakesPrecedence(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(binaryDir, "config.yml"), validConfig)
	writeFile(t, filepath.Join(home, ".config", "hostprobe", "config.yml"), "not: valid")

	path, config, err := loadConfig(binaryDir, home)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(binaryDir, "config.yml") || config.Database.Host != "db.example" {
		t.Fatalf("unexpected selection: %s %+v", path, config)
	}
	if config.Settings.SSHTimeout != 2500*time.Millisecond {
		t.Fatalf("unexpected timeout: %v", config.Settings.SSHTimeout)
	}
}

func TestInvalidSelectedConfigDoesNotFallBack(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(binaryDir, "config.yml"), "database: [\npassword: hunter2")
	writeFile(t, filepath.Join(home, ".config", "hostprobe", "config.yml"), validConfig)

	_, _, err := loadConfig(binaryDir, home)
	if err == nil || !strings.Contains(err.Error(), "invalid YAML") {
		t.Fatalf("expected YAML error, got %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error echoes file content: %v", err)
	}
}

func TestFirstRunCreatesPrivateTemplate(t *testing.T) {
	binaryDir, home := t.TempDir(), t.TempDir()
	_, _, err := loadConfig(binaryDir, home)
	var created *configCreatedError
	if !errors.As(err, &created) {
		t.Fatalf("expected template creation, got %v", err)
	}
	info, err := os.Stat(created.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("template not private: %v %v", info, err)
	}
	// Template must be rejected until edited
	if _, _, err := loadConfig(binaryDir, home); err == nil || !strings.Contains(err.Error(), "placeholder") {
		t.Fatalf("expected placeholder error, got %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	cases := map[string][2]string{
		"bad port":       {`port: 5432`, `port: 70000`},
		"boolean limit":  {`ping_concurrency: 16`, `ping_concurrency: true`},
		"zero timeout":   {`ping_timeout_seconds: 1`, `ping_timeout_seconds: 0`},
		"blank password": {`password: "secret"`, `password: " "`},
		"unknown key":    {`ssh_concurrency: 8`, "ssh_concurrency: 8\n  table: x"},
		"missing key":    {"  ssh_concurrency: 8\n", ""},
		"four backups":   {`password: "secret"`, "password: \"secret\"\n  backups: [1, 2, 3, 4]"},
		"backup key":     {`password: "secret"`, "password: \"secret\"\n  backups: [{host: a}]"},
	}
	for name, replace := range cases {
		if _, err := parseYAML(t, strings.Replace(validConfig, replace[0], replace[1], 1)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	backup := "password: \"secret\"\n  backups:\n    - {host: b, port: 1, name: n, user: u, password: p}"
	config, err := parseYAML(t, strings.Replace(validConfig, `password: "secret"`, backup, 1))
	if err != nil || len(config.Backups) != 1 || config.Backups[0].Host != "b" {
		t.Fatalf("backup not parsed: %+v %v", config, err)
	}
}

func TestValidateRecord(t *testing.T) {
	record, reason := validateRecord(RawRecord{ID: int32(1), Name: " web ", Method: " PING ", IP: " 192.0.2.1 "})
	if reason != "" || record.Name != "web" || record.Method != "ping" || record.IP != "192.0.2.1" || record.IPVersion != 4 {
		t.Fatalf("unexpected ping record: %+v %q", record, reason)
	}
	record, reason = validateRecord(RawRecord{
		ID: int32(2), Name: "db", Method: "ssh", IP: "2001:DB8::1", SSHUser: " ops ", SSHPort: int32(2222),
	})
	if reason != "" || record.IP != "2001:db8::1" || record.IPVersion != 6 || record.SSHUser != "ops" || record.SSHPort != 2222 {
		t.Fatalf("unexpected ssh record: %+v %q", record, reason)
	}

	invalid := map[string]RawRecord{
		"name is missing":                             {ID: 1, Name: nil, Method: "ping", IP: "192.0.2.1"},
		"method is missing":                           {ID: 1, Name: "a", Method: nil, IP: "192.0.2.1"},
		"unknown method: http":                        {ID: 1, Name: "a", Method: "http", IP: "192.0.2.1"},
		"target IP address is missing":                {ID: 1, Name: "a", Method: "ping", IP: " "},
		"target is not a valid IPv4 or IPv6 address":  {ID: 1, Name: "a", Method: "ping", IP: "example.com"},
		"ssh_user is missing":                         {ID: 1, Name: "a", Method: "ssh", IP: "192.0.2.1", SSHPort: int32(22)},
		"ssh_port must be an integer from 1 to 65535": {ID: 1, Name: "a", Method: "ssh", IP: "192.0.2.1", SSHUser: "u", SSHPort: int32(0)},
	}
	for expected, raw := range invalid {
		if _, reason := validateRecord(raw); reason != expected {
			t.Errorf("expected %q, got %q", expected, reason)
		}
	}
}

func TestStatusFormat(t *testing.T) {
	rows := func(values ...any) []RawRecord {
		var records []RawRecord
		for _, value := range values {
			records = append(records, RawRecord{Status: value})
		}
		return records
	}
	valid := []struct {
		dataType string
		records  []RawRecord
		format   statusFormat
		stored   any
	}{
		{"text", nil, statusWords, "online"},
		{"character varying", rows("Online", " offline "), statusWords, "online"},
		{"text", rows("TRUE", "false"), statusBooleanText, "true"},
		{"boolean", rows(true, false), statusBoolean, true},
	}
	for _, test := range valid {
		format, err := inferStatusFormat(test.dataType, test.records)
		if err != nil || format != test.format || storedStatus(true, format) != test.stored {
			t.Errorf("%s %v: got %v %v", test.dataType, test.records, format, err)
		}
	}
	invalid := []struct {
		dataType string
		records  []RawRecord
	}{
		{"boolean", rows(true, nil)},
		{"integer", nil},
		{"text", rows("online", nil)},
		{"text", rows("online", "true")},
		{"text", rows("up")},
	}
	for _, test := range invalid {
		if _, err := inferStatusFormat(test.dataType, test.records); err == nil {
			t.Errorf("%s %v: expected an error", test.dataType, test.records)
		}
	}
}

func TestFailureCategories(t *testing.T) {
	ssh := map[string]CheckResult{
		"Host key verification failed.":      {stateOffline, "host-key verification failed"},
		"user@host: Permission denied (pub)": {stateOffline, "authentication failed"},
		"connect: Connection refused":        {stateOffline, "connection refused"},
		"Connection timed out":               {stateOffline, "connection timeout"},
		"connect: No route to host":          {stateOffline, "target unreachable"},
		"something else":                     {stateError, "SSH client failed"},
	}
	for stderr, expected := range ssh {
		if result := sshFailure(stderr); result != expected {
			t.Errorf("%q: got %+v", stderr, result)
		}
	}
	if pingFailure("ping: socket: Operation not permitted") != "local ping permission denied" ||
		pingFailure("Destination Host Unreachable") != "target unreachable" ||
		pingFailure("") != "no echo reply" {
		t.Error("unexpected ping failure category")
	}
}

// Fake ping and ssh executables on PATH
func fakeTools(t *testing.T, ping, ssh string) {
	t.Helper()
	directory := t.TempDir()
	for name, body := range map[string]string{"ping": ping, "ssh": ssh} {
		if body == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
}

func TestChecksRunExternalTools(t *testing.T) {
	// Target selects the simulated behavior
	fakeTools(t,
		`for target; do :; done
case "$target" in
  192.0.2.1) exit 0 ;;
  192.0.2.2) echo "Destination Host Unreachable" >&2; exit 1 ;;
  192.0.2.3) echo "ping: socket: Operation not permitted" >&2; exit 2 ;;
  *) exec /bin/sleep 5 ;;
esac`,
		`[ "$1 $2 $3 $4" = "-N -T -M -S" ] || exit 9
for target; do :; done
case "$target" in
  192.0.2.1) : > "$5"; exec /bin/sleep 5 ;;
  192.0.2.2) echo "Permission denied (publickey)." >&2; exit 255 ;;
  *) exec /bin/sleep 5 ;;
esac`)

	var records []HostRecord
	for _, method := range []string{"ping", "ssh"} {
		for _, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.3", "192.0.2.4"} {
			records = append(records, HostRecord{Method: method, IP: ip, IPVersion: 4, SSHUser: "u", SSHPort: 22})
		}
	}
	settings := Settings{PingConcurrency: 2, PingTimeout: 300 * time.Millisecond, SSHConcurrency: 4, SSHTimeout: 300 * time.Millisecond}
	started := time.Now()
	checked := checkHosts(context.Background(), records, settings)

	expected := []CheckResult{
		{stateOnline, "echo reply received"},
		{stateOffline, "target unreachable"},
		{stateError, "local ping permission denied"},
		{stateOffline, "timeout"},
		{stateOnline, "SSH authentication succeeded"},
		{stateOffline, "authentication failed"},
		{stateOffline, "timeout"},
		{stateOffline, "timeout"},
	}
	for index, item := range checked {
		if item.Result != expected[index] {
			t.Errorf("%s %s: got %+v, want %+v", item.Record.Method, item.Record.IP, item.Result, expected[index])
		}
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Errorf("deadlines not enforced: %v", elapsed)
	}
}

func TestMissingToolIsOperationalError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	settings := Settings{PingConcurrency: 1, PingTimeout: time.Second, SSHConcurrency: 1, SSHTimeout: time.Second}
	checked := checkHosts(context.Background(), []HostRecord{{Method: "ping", IP: "192.0.2.1", IPVersion: 4}}, settings)
	if checked[0].Result != (CheckResult{stateError, "missing local dependency: ping"}) {
		t.Fatalf("unexpected result: %+v", checked[0].Result)
	}
}
