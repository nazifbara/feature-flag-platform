package applogger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/nazifbara/feature-flag-platform/api/internal/apperr"
	pkgerr "github.com/pkg/errors"
)

// logOne logs msg with args through a JSON handler using replaceLogAttr, and
// returns the decoded entry without its time and level.
func logOne(t *testing.T, msg string, args ...any) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: replaceLogAttr}))
	logger.Info(msg, args...)
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", buf.String(), err)
	}
	delete(entry, "time")
	delete(entry, "level")
	return entry
}

func TestReplaceLogAttrRedactsSensitiveKeys(t *testing.T) {
	for _, key := range sensitiveKeys {
		t.Run(key, func(t *testing.T) {
			entry := logOne(t, "hi", key, "hunter2")
			if entry[key] != "[REDACTED]" {
				t.Errorf("%s = %v, want [REDACTED]", key, entry[key])
			}
		})
	}
	// Redaction applies inside groups too.
	entry := logOne(t, "hi", slog.Group("db", "password", "hunter2"))
	if got := entry["db"].(map[string]any)["password"]; got != "[REDACTED]" {
		t.Errorf("db.password = %v, want [REDACTED]", got)
	}
	// A non-string value is redacted as well.
	entry = logOne(t, "hi", "pin", 1234)
	if entry["pin"] != "[REDACTED]" {
		t.Errorf("pin = %v, want [REDACTED]", entry["pin"])
	}
}

// Keys match whatever their case.
func TestReplaceLogAttrRedactsAnyCase(t *testing.T) {
	for _, key := range []string{"Password", "PASSWORD", "apiKey", "API_KEY", "Authorization"} {
		t.Run(key, func(t *testing.T) {
			entry := logOne(t, "hi", key, "hunter2")
			if entry[key] != "[REDACTED]" {
				t.Errorf("%s = %v, want [REDACTED]", key, entry[key])
			}
		})
	}
}

// Identifiers like a flag's key or a user aren't secret, and are what a log is
// read for.
func TestReplaceLogAttrKeepsIdentifiers(t *testing.T) {
	for _, key := range []string{"key", "user", "flag_key", "user_id", "email"} {
		t.Run(key, func(t *testing.T) {
			entry := logOne(t, "hi", key, "dark-mode")
			if entry[key] != "dark-mode" {
				t.Errorf("%s = %v, want dark-mode", key, entry[key])
			}
		})
	}
}

func TestReplaceLogAttrRedactsURLPasswords(t *testing.T) {
	tests := map[string]struct{ value, want string }{
		"alone": {
			"postgres://admin:hunter2@db.local:5432/flags?sslmode=disable",
			"postgres://admin:xxxxx@db.local:5432/flags?sslmode=disable",
		},
		// A URL inside a sentence, like an error's text, is redacted too.
		"in a sentence": {
			"can't connect to postgres://admin:hunter2@db.local/flags: connection refused",
			"can't connect to postgres://admin:xxxxx@db.local/flags: connection refused",
		},
		"two URLs": {
			"from https://a:one@a.local to https://b:two@b.local",
			"from https://a:xxxxx@a.local to https://b:xxxxx@b.local",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			entry := logOne(t, test.value, "reason", test.value)
			if entry["msg"] != test.want {
				t.Errorf("msg = %q, want %q", entry["msg"], test.want)
			}
			if entry["reason"] != test.want {
				t.Errorf("reason = %q, want %q", entry["reason"], test.want)
			}
		})
	}
}

// Only URLs with a password are rewritten; any other text is logged as given.
func TestReplaceLogAttrKeepsOtherStrings(t *testing.T) {
	values := []string{
		"can't create the rule",
		"dark mode",
		`pq: relation "rules" does not exist`,
		"https://example.com/a b",
		"postgres://admin@db.local/flags",
		"100% done",
		// Words url.Parse rejects.
		"%zz",
		"http://[::1",
		// Spacing is kept as given.
		"two  spaces",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			entry := logOne(t, value, "reason", value)
			if entry["msg"] != value {
				t.Errorf("msg = %q, want %q", entry["msg"], value)
			}
			if entry["reason"] != value {
				t.Errorf("reason = %q, want %q", entry["reason"], value)
			}
		})
	}
}

func TestReplaceLogAttrExpandsErrors(t *testing.T) {
	tests := map[string]struct {
		err  error
		want map[string]any
	}{
		"plain": {
			err:  errors.New("boom"),
			want: map[string]any{"error": map[string]any{"message": "boom"}},
		},
		"with attributes": {
			err:  fmt.Errorf("creating: %w", apperr.WithAttrs(errors.New("boom"), "rule_id", 7, "flag", "dark-mode")),
			want: map[string]any{"error": map[string]any{"message": "creating: boom", "rule_id": 7.0, "flag": "dark-mode"}},
		},
		"joined": {
			err: errors.Join(errors.New("first"), apperr.WithAttrs(errors.New("second"), "rule_id", 7)),
			want: map[string]any{"errors": map[string]any{
				"message": "first\nsecond",
				"error_1": map[string]any{"message": "first"},
				"error_2": map[string]any{"message": "second", "rule_id": 7.0},
			}},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			entry := logOne(t, "failed", "error", test.err)
			delete(entry, "msg")
			if diff := cmp.Diff(test.want, entry); diff != "" {
				t.Errorf("entry mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A connect error's text leaves out its cause, but the log must show it: it's
// what the request actually failed on.
func TestReplaceLogAttrLogsConnectCauses(t *testing.T) {
	cause := apperr.WithAttrs(errors.New(`pq: relation "rules" does not exist`), "rule_id", 7)
	err := fmt.Errorf("creating: %w", connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(cause))
	entry := logOne(t, "failed", "error", err)
	raw, _ := json.Marshal(entry["error"])
	for _, want := range []string{"creating: internal: can't create the rule", `pq: relation \"rules\" does not exist`, `"rule_id":7`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("error = %s, want it to contain %s", raw, want)
		}
	}
}

func TestReplaceLogAttrLogsHiddenCauses(t *testing.T) {
	dbErr := errors.New(`pq: relation "rules" does not exist`)
	tests := map[string]struct {
		err error
		// wantCause is the logged cause. Empty means none is logged.
		wantCause string
	}{
		"connect error with a cause": {
			err:       connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(dbErr),
			wantCause: dbErr.Error(),
		},
		// A cause that shows its own causes is logged whole.
		"cause wrapping more": {
			err:       connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(fmt.Errorf("inserting: %w", dbErr)),
			wantCause: "inserting: " + dbErr.Error(),
		},
		// Each layer that hides its cause adds it.
		"hidden twice": {
			err: connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(
				fmt.Errorf("in a transaction: %w", connect.NewError(connect.CodeNotFound, "no such setting").WithCause(dbErr))),
			wantCause: "in a transaction: not_found: no such setting: " + dbErr.Error(),
		},
		"connect error without a cause": {
			err: connect.NewError(connect.CodeInvalidArgument, "bad name"),
		},
		// Every wrapped text is already in the message.
		"wrapped plain error": {
			err: fmt.Errorf("creating: %w", fmt.Errorf("inserting: %w", dbErr)),
		},
		"attributes only": {
			err: apperr.WithAttrs(dbErr, "rule_id", 7),
		},
		"plain error": {
			err: dbErr,
		},
		"cause with a database URL": {
			err:       connect.NewError(connect.CodeUnavailable, "can't reach the database").WithCause(errors.New("dial postgres://admin:hunter2@db.local/flags failed")),
			wantCause: "dial postgres://admin:xxxxx@db.local/flags failed",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			entry := logOne(t, "failed", "error", test.err)
			group, _ := entry["error"].(map[string]any)
			if group["message"] != test.err.Error() {
				t.Errorf("error.message = %v, want %q", group["message"], test.err.Error())
			}
			got, logged := group["cause"]
			if test.wantCause == "" && logged {
				t.Errorf("error.cause = %q, want none", got)
			}
			if test.wantCause != "" && got != test.wantCause {
				t.Errorf("error.cause = %v, want %q", got, test.wantCause)
			}
		})
	}
}

// The message is redacted too.
func TestReplaceLogAttrRedactsErrorMessages(t *testing.T) {
	entry := logOne(t, "failed", "error", errors.New("dial postgres://admin:hunter2@db.local/flags failed"))
	group, _ := entry["error"].(map[string]any)
	if want := "dial postgres://admin:xxxxx@db.local/flags failed"; group["message"] != want {
		t.Errorf("error.message = %v, want %q", group["message"], want)
	}
}

// Attributes added to an error are redacted like any other.
func TestReplaceLogAttrRedactsErrorAttrs(t *testing.T) {
	err := apperr.WithAttrs(errors.New("can't sign in"), "password", "hunter2", "db_url", "postgres://admin:hunter2@db.local/flags")
	entry := logOne(t, "failed", "error", err)
	group, _ := entry["error"].(map[string]any)
	if group["password"] != "[REDACTED]" {
		t.Errorf("error.password = %v, want [REDACTED]", group["password"])
	}
	if want := "postgres://admin:xxxxx@db.local/flags"; group["db_url"] != want {
		t.Errorf("error.db_url = %v, want %q", group["db_url"], want)
	}
}

// An error wrapping a join is logged both ways: as the outer error, with its
// message and attributes, and as each joined error with its own.
func TestReplaceLogAttrLogsWrappedJoins(t *testing.T) {
	joined := errors.Join(errors.New("first"), apperr.WithAttrs(errors.New("second"), "rule_id", 7))
	err := fmt.Errorf("creating: %w", apperr.WithAttrs(joined, "flag", "dark-mode"))
	entry := logOne(t, "failed", "error", err)

	// Where each part goes is up to the logger, so look for them anywhere in
	// the line.
	objects := groupsIn(entry)
	if !slices.ContainsFunc(objects, func(o map[string]any) bool { return o["message"] == err.Error() && o["flag"] == "dark-mode" }) {
		t.Errorf("no group with message %q and flag dark-mode in %v", err.Error(), entry)
	}
	if !slices.ContainsFunc(objects, func(o map[string]any) bool { return o["message"] == "first" }) {
		t.Errorf("no group with message first in %v", entry)
	}
	if !slices.ContainsFunc(objects, func(o map[string]any) bool { return o["message"] == "second" && o["rule_id"] == 7.0 }) {
		t.Errorf("no group with message second and rule_id 7 in %v", entry)
	}
}

// A cause hidden between the outer error and the join, like a connect error's,
// is logged once, and the outer text isn't repeated.
func TestReplaceLogAttrLogsCausesAboveJoins(t *testing.T) {
	joined := errors.Join(errors.New("first"), errors.New("second"))
	err := fmt.Errorf("creating: %w", connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(fmt.Errorf("in a transaction: %w", joined)))
	entry := logOne(t, "failed", "error", err)
	raw, _ := json.Marshal(entry)
	line := string(raw)
	hidden, _ := json.Marshal("in a transaction: first\nsecond")
	if !strings.Contains(line, strings.Trim(string(hidden), `"`)) {
		t.Errorf("line %s, want it to contain the hidden cause %s", line, hidden)
	}
	if n := strings.Count(line, "creating: internal: can't create the rule"); n != 1 {
		t.Errorf("line %s has the outer text %d times, want 1", line, n)
	}
}

// A joined error's stack trace is logged on its own entry only, not again on
// the outer error.
func TestReplaceLogAttrLogsJoinedStackTracesOnce(t *testing.T) {
	err := fmt.Errorf("creating: %w", errors.Join(errors.New("first"), pkgerr.New("second")))
	entry := logOne(t, "failed", "error", err)
	var withTrace []map[string]any
	for _, group := range groupsIn(entry) {
		if _, ok := group["stack_trace"]; ok {
			withTrace = append(withTrace, group)
		}
	}
	if len(withTrace) != 1 {
		t.Fatalf("%d groups with a stack trace, want 1: %v", len(withTrace), entry)
	}
	if withTrace[0]["message"] != "second" {
		t.Errorf("stack trace logged on %v, want the group of second", withTrace[0]["message"])
	}
}

// groupsIn returns v's groups, at any depth, v included.
func groupsIn(v any) []map[string]any {
	group, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	groups := []map[string]any{group}
	for _, inner := range group {
		groups = append(groups, groupsIn(inner)...)
	}
	return groups
}

func TestReplaceLogAttrLogsStackTraces(t *testing.T) {
	entry := logOne(t, "failed", "error", fmt.Errorf("creating: %w", pkgerr.New("boom")))
	group, _ := entry["error"].(map[string]any)
	if group["message"] != "creating: boom" {
		t.Errorf("error.message = %v, want %q", group["message"], "creating: boom")
	}
	trace, _ := group["stack_trace"].(string)
	if !strings.Contains(trace, "TestReplaceLogAttrLogsStackTraces") {
		t.Errorf("error.stack_trace = %q, want the frame that made the error", trace)
	}
}

// Only an error value under "error" is expanded.
func TestReplaceLogAttrLeavesNonErrors(t *testing.T) {
	entry := logOne(t, "failed", "error", 42, "cause", errors.New("boom"))
	if entry["error"] != 42.0 {
		t.Errorf("error = %v, want 42", entry["error"])
	}
	if entry["cause"] != "boom" {
		t.Errorf("cause = %v, want boom", entry["cause"])
	}
}

func TestInitializeLoggerWritesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	logger, closeLogger, err := InitializeLogger(path)
	if err != nil {
		t.Fatalf("InitializeLogger: %v", err)
	}
	logger.Debug("only on stderr")
	logger.Info("served", "password", "hunter2")
	if err := closeLogger(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 {
		t.Fatalf("log file has %d lines, want 1 (no debug): %q", len(lines), raw)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decoding log line %q: %v", lines[0], err)
	}
	if entry["msg"] != "served" {
		t.Errorf("msg = %v, want served", entry["msg"])
	}
	if entry["password"] != "[REDACTED]" {
		t.Errorf("password = %v, want [REDACTED]", entry["password"])
	}
}

func TestInitializeLoggerWithoutFile(t *testing.T) {
	logger, closeLogger, err := InitializeLogger("")
	if err != nil {
		t.Fatalf("InitializeLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("logger is nil")
	}
	if err := closeLogger(); err != nil {
		t.Errorf("closing: %v", err)
	}
}
