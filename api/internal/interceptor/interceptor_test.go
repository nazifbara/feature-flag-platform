package interceptor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/google/go-cmp/cmp"
	pingv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/ping/v1"
	"github.com/nazifbara/feature-flag-platform/gen/go/ffp/ping/v1/pingv1connect"
)

// failingPing answers every call with err, or with a response if err is nil.
type failingPing struct{ err error }

func (p failingPing) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	if p.err != nil {
		return nil, p.err
	}
	return &pingv1.PingResponse{Message: req.Message}, nil
}

// recorder is a slog.Handler keeping the values logged under "error", so tests
// can check it's an error the logger can expand, not just its text.
type recorder struct{ errs *[]any }

func (r recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r recorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r recorder) WithGroup(string) slog.Handler            { return r }
func (r recorder) Handle(_ context.Context, rec slog.Record) error {
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			*r.errs = append(*r.errs, a.Value.Any())
		}
		return true
	})
	return nil
}

// call sends a ping through a real server with the interceptor, and returns
// the response's status, its body, the log lines written meanwhile, and the
// values logged under "error".
func call(t *testing.T, err error) (status int, body string, logs []map[string]any, errs []any) {
	t.Helper()
	status, body, logs, errs, _ = callWithHeader(t, err, nil)
	return status, body, logs, errs
}

// callWithHeader is call sending header with the request, and also returns
// the response's header.
func callWithHeader(t *testing.T, err error, header http.Header) (status int, body string, logs []map[string]any, errs []any, resHeader http.Header) {
	t.Helper()
	return serve(t, failingPing{err: err}, header, true)
}

// idPing answers every call, keeping what GetRequestID gives in its context.
type idPing struct {
	called *bool
	id     *uuid.UUID
	err    *error
}

func (p idPing) Ping(ctx context.Context, req *pingv1.PingRequest) (*pingv1.PingResponse, error) {
	*p.called = true
	*p.id, *p.err = GetRequestID(ctx)
	return &pingv1.PingResponse{Message: req.Message}, nil
}

// serve sends a ping with header to handler through a real server with the
// logging interceptor, behind the request ID one as in cmd/admin when
// withRequestID is set, and returns what callWithHeader does.
func serve(t *testing.T, handler pingv1connect.PingServiceHandler, header http.Header, withRequestID bool) (status int, body string, logs []map[string]any, errs []any, resHeader http.Header) {
	t.Helper()
	var logged bytes.Buffer
	logger := slog.New(slog.NewMultiHandler(slog.NewJSONHandler(&logged, nil), recorder{&errs}))

	var server *connect.Server
	if withRequestID {
		server = connect.NewServer(NewRequestIDInterceptor(), NewLoggingInterceptor(logger))
	} else {
		server = connect.NewServer(NewLoggingInterceptor(logger))
	}
	pingv1connect.RegisterPingServiceHandler(server, handler)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, reqErr := http.NewRequest(http.MethodPost, ts.URL+pingv1connect.PingServicePingProcedure, strings.NewReader(`{"message":"hi"}`))
	if reqErr != nil {
		t.Fatalf("building the request: %v", reqErr)
	}
	for key, values := range header {
		req.Header[key] = values
	}
	req.Header.Set("Content-Type", "application/json")
	res, postErr := http.DefaultClient.Do(req)
	if postErr != nil {
		t.Fatalf("calling the server: %v", postErr)
	}
	defer res.Body.Close()
	raw, readErr := io.ReadAll(res.Body)
	if readErr != nil {
		t.Fatalf("reading the response: %v", readErr)
	}

	for line := range strings.Lines(logged.String()) {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		// Decoding keeps only the last of a repeated key, which would hide
		// that one was logged twice.
		if keys := repeatedKeys(t, line); len(keys) > 0 {
			t.Errorf("log line %q repeats keys %v", line, keys)
		}
		logs = append(logs, entry)
	}
	return res.StatusCode, string(raw), logs, errs, res.Header
}

// repeatedKeys returns the top-level keys that appear more than once in the
// JSON object line.
func repeatedKeys(t *testing.T, line string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(line))
	if _, err := dec.Token(); err != nil {
		t.Fatalf("decoding log line %q: %v", line, err)
	}
	seen := map[string]bool{}
	var repeated []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
		key := tok.(string)
		if seen[key] {
			repeated = append(repeated, key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("decoding log line %q: %v", line, err)
		}
	}
	return repeated
}

func TestLoggingInterceptorLogsFailures(t *testing.T) {
	cause := errors.New(`pq: relation "secret_table" does not exist`)
	const secret = "secret_table"

	tests := map[string]struct {
		err error
		// wantCause is the cause the logged error must lead to, if any.
		wantCause error
		// wantClient is the message the client gets.
		wantClient string
	}{
		"internal with a cause": {
			err:        connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(cause),
			wantCause:  cause,
			wantClient: "can't create the rule",
		},
		"already exists with a cause": {
			err:        connect.NewError(connect.CodeAlreadyExists, "can't create the rule").WithCause(cause),
			wantCause:  cause,
			wantClient: "can't create the rule",
		},
		"not found with a cause": {
			err:        connect.NewError(connect.CodeNotFound, "can't get the rule").WithCause(cause),
			wantCause:  cause,
			wantClient: "can't get the rule",
		},
		"connect error without a cause": {
			err:        connect.NewError(connect.CodeInvalidArgument, "bad name"),
			wantClient: "bad name",
		},
		// The cause is found through any wrapping.
		"wrapped connect error": {
			err:        fmt.Errorf("signing up: %w", connect.NewError(connect.CodeInternal, "can't sign up").WithCause(cause)),
			wantCause:  cause,
			wantClient: "can't sign up",
		},
		// connect keeps a plain error's text from the client.
		"plain wrapped error": {
			err:        fmt.Errorf("outer: %w", errors.New("inner")),
			wantClient: "",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, body, logs, errs := call(t, test.err)
			if len(logs) != 1 {
				t.Fatalf("%d log lines, want 1: %v", len(logs), logs)
			}
			entry := logs[0]
			if entry["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", entry["level"])
			}
			if entry["msg"] != "served request" {
				t.Errorf("msg = %q, want %q", entry["msg"], "served request")
			}
			// The whole error is logged as an error, so the logger can expand
			// it, and nothing is lost: its wrapping, code, message or cause.
			if len(errs) != 1 {
				t.Fatalf("logged %d errors, want 1: %v", len(errs), errs)
			}
			got, ok := errs[0].(error)
			if !ok {
				t.Fatalf("error = %T %v, want an error", errs[0], errs[0])
			}
			if !errors.Is(got, test.err) {
				t.Errorf("error = %v, want it to wrap %v", got, test.err)
			}
			if test.wantCause != nil && !errors.Is(got, test.wantCause) {
				t.Errorf("error = %v, want it to lead to the cause %v", got, test.wantCause)
			}
			if entry["procedure"] != pingv1connect.PingServicePingProcedure {
				t.Errorf("procedure = %v, want %q", entry["procedure"], pingv1connect.PingServicePingProcedure)
			}
			if addr, _ := entry["caller-address"].(string); addr == "" {
				t.Errorf("caller-address = %v, want an address", entry["caller-address"])
			}

			var res struct{ Message string }
			if err := json.Unmarshal([]byte(body), &res); err != nil {
				t.Fatalf("decoding response %q: %v", body, err)
			}
			if diff := cmp.Diff(test.wantClient, res.Message); diff != "" {
				t.Errorf("client message mismatch (-want +got):\n%s", diff)
			}
			// The cause is for the log, never for the client.
			if strings.Contains(body, secret) {
				t.Errorf("response %q reveals the cause", body)
			}
		})
	}
}

// A successful call is logged too, at info, so failures stand out by level.
func TestLoggingInterceptorLogsSuccess(t *testing.T) {
	status, _, logs, errs := call(t, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(logs) != 1 {
		t.Fatalf("%d log lines, want 1: %v", len(logs), logs)
	}
	entry := logs[0]
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", entry["level"])
	}
	if entry["msg"] != "served request" {
		t.Errorf("msg = %q, want %q", entry["msg"], "served request")
	}
	if entry["procedure"] != pingv1connect.PingServicePingProcedure {
		t.Errorf("procedure = %v, want %q", entry["procedure"], pingv1connect.PingServicePingProcedure)
	}
	if addr, _ := entry["caller-address"].(string); addr == "" {
		t.Errorf("caller-address = %v, want an address", entry["caller-address"])
	}
	if len(errs) != 0 {
		t.Errorf("logged errors %v for a successful call, want none", errs)
	}
}

func TestLoggingInterceptorRequestID(t *testing.T) {
	tests := map[string]error{
		"success": nil,
		"failure": connect.NewError(connect.CodeInternal, "can't create the rule"),
	}
	for name, callErr := range tests {
		// A client's ID is logged and sent back as is, so it can match its
		// own logs with the server's.
		t.Run(name+" with an ID", func(t *testing.T) {
			const clientID = "0b9b8f4e-3c1d-4a7e-9f2a-5d6c7e8f9a0b"
			_, _, logs, _, header := callWithHeader(t, callErr, http.Header{"X-Request-Id": {clientID}})
			if len(logs) != 1 {
				t.Fatalf("%d log lines, want 1: %v", len(logs), logs)
			}
			if logs[0]["request-id"] != clientID {
				t.Errorf("request-id = %v, want %s", logs[0]["request-id"], clientID)
			}
			if got := header.Get("X-Request-Id"); got != clientID {
				t.Errorf("response X-Request-Id = %q, want %s", got, clientID)
			}
		})
		// Without one, the server makes one up and sends it back.
		t.Run(name+" without an ID", func(t *testing.T) {
			_, _, logs, _, header := callWithHeader(t, callErr, nil)
			if len(logs) != 1 {
				t.Fatalf("%d log lines, want 1: %v", len(logs), logs)
			}
			id, _ := logs[0]["request-id"].(string)
			if _, err := uuid.Parse(id); err != nil {
				t.Fatalf("request-id = %q, want a UUID: %v", id, err)
			}
			if got := header.Get("X-Request-Id"); got != id {
				t.Errorf("response X-Request-Id = %q, want the logged %q", got, id)
			}
		})
	}
}

// Each call without an ID gets its own.
func TestLoggingInterceptorMakesDistinctRequestIDs(t *testing.T) {
	_, _, first, _ := call(t, nil)
	_, _, second, _ := call(t, nil)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("log lines %v and %v, want one each", first, second)
	}
	if first[0]["request-id"] == second[0]["request-id"] {
		t.Errorf("both calls got request-id %v, want different IDs", first[0]["request-id"])
	}
}

// An ID that isn't a UUID is replaced with a new one, which is logged and sent
// back in its place, so a client's bad header never reaches the logs.
func TestLoggingInterceptorReplacesInvalidRequestIDs(t *testing.T) {
	const bad = "req-123"
	for name, callErr := range map[string]error{
		"success": nil,
		"failure": connect.NewError(connect.CodeInternal, "can't create the rule"),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, logs, _, header := callWithHeader(t, callErr, http.Header{"X-Request-Id": {bad}})
			served := servedLine(t, logs)
			id, _ := served["request-id"].(string)
			parsed, err := uuid.Parse(id)
			if err != nil {
				t.Fatalf("request-id = %q, want a UUID: %v", id, err)
			}
			if parsed == (uuid.UUID{}) {
				t.Errorf("request-id = %q, want a new UUID, not the nil one", id)
			}
			if got := header.Get("X-Request-Id"); got != id {
				t.Errorf("response X-Request-Id = %q, want the logged %q", got, id)
			}
			for _, entry := range logs {
				for key, value := range entry {
					if value == bad {
						t.Errorf("logged the bad ID under %q in %v", key, entry)
					}
				}
			}
		})
	}
}

// Replaced IDs are new each time, so two bad requests don't share one.
func TestLoggingInterceptorReplacesInvalidRequestIDsDistinctly(t *testing.T) {
	header := http.Header{"X-Request-Id": {"req-123"}}
	_, _, first, _, _ := callWithHeader(t, nil, header)
	_, _, second, _, _ := callWithHeader(t, nil, header)
	a, b := servedLine(t, first)["request-id"], servedLine(t, second)["request-id"]
	if a == b {
		t.Errorf("both calls got request-id %v, want different IDs", a)
	}
}

// servedLine returns the "served request" line of logs.
func servedLine(t *testing.T, logs []map[string]any) map[string]any {
	t.Helper()
	for _, entry := range logs {
		if entry["msg"] == "served request" {
			return entry
		}
	}
	t.Fatalf("no served request line in %v", logs)
	return nil
}

// Every form uuid.Parse accepts is logged and sent back in the standard one, so
// one ID is one string in the logs.
func TestLoggingInterceptorStandardizesRequestIDs(t *testing.T) {
	const want = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	for _, sent := range []string{
		want,
		"F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6",
		"{f81d4fae-7dec-11d0-a765-00a0c91e6bf6}",
		"urn:uuid:f81d4fae-7dec-11d0-a765-00a0c91e6bf6",
		"f81d4fae7dec11d0a76500a0c91e6bf6",
	} {
		t.Run(sent, func(t *testing.T) {
			_, _, logs, _, header := callWithHeader(t, nil, http.Header{"X-Request-Id": {sent}})
			if got := servedLine(t, logs)["request-id"]; got != want {
				t.Errorf("request-id = %v, want %s", got, want)
			}
			if got := header.Get("X-Request-Id"); got != want {
				t.Errorf("response X-Request-Id = %q, want %s", got, want)
			}
		})
	}
}

// Generated IDs are version 7, so they sort in the order the calls came in.
func TestLoggingInterceptorMakesOrderedRequestIDs(t *testing.T) {
	var ids []uuid.UUID
	for _, header := range []http.Header{nil, {"X-Request-Id": {"req-123"}}, nil} {
		_, _, logs, _, _ := callWithHeader(t, nil, header)
		id := uuid.MustParse(servedLine(t, logs)["request-id"].(string))
		if version := id[6] >> 4; version != 7 {
			t.Errorf("request-id %s is version %d, want 7", id, version)
		}
		ids = append(ids, id)
	}
	for i := 1; i < len(ids); i++ {
		if ids[i-1].Compare(ids[i]) >= 0 {
			t.Errorf("request-id %s doesn't sort after the earlier %s", ids[i], ids[i-1])
		}
	}
}

// Handlers read the call's request ID from their context, to record it with
// what the call changes, so it must be the one logged and sent back: the
// client's in the standard form, or the one made up in its place.
func TestRequestIDInterceptorPutsIDInContext(t *testing.T) {
	const clientID = "f81d4fae-7dec-11d0-a765-00a0c91e6bf6"
	tests := map[string]struct {
		header http.Header
		want   string // "" when a new ID is made up
	}{
		"client's ID":             {header: http.Header{"X-Request-Id": {clientID}}, want: clientID},
		"client's ID, other form": {header: http.Header{"X-Request-Id": {"F81D4FAE-7DEC-11D0-A765-00A0C91E6BF6"}}, want: clientID},
		"no ID":                   {},
		"invalid ID":              {header: http.Header{"X-Request-Id": {"req-123"}}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var called bool
			var id uuid.UUID
			var idErr error
			status, _, logs, _, header := serve(t, idPing{&called, &id, &idErr}, test.header, true)
			if status != http.StatusOK || !called {
				t.Fatalf("status = %d, handler called = %v, want %d and called", status, called, http.StatusOK)
			}
			if idErr != nil {
				t.Fatalf("GetRequestID() in the handler: %v", idErr)
			}
			if test.want != "" && id.String() != test.want {
				t.Errorf("handler's request ID = %s, want %s", id, test.want)
			}
			if test.want == "" {
				if version := id[6] >> 4; version != 7 {
					t.Errorf("handler's request ID %s is version %d, want a new version 7 UUID", id, version)
				}
			}
			if got := servedLine(t, logs)["request-id"]; got != id.String() {
				t.Errorf("logged request-id = %v, want the handler's %s", got, id)
			}
			if got := header.Get("X-Request-Id"); got != id.String() {
				t.Errorf("response X-Request-Id = %q, want the handler's %s", got, id)
			}
		})
	}
}

// Code run outside a call has no request ID, and must be told so rather than
// get one that looks real.
func TestGetRequestIDWithoutOne(t *testing.T) {
	id, err := GetRequestID(context.Background())
	if err == nil {
		t.Fatalf("GetRequestID() = %s, want an error", id)
	}
	if id != uuid.Nil() {
		t.Errorf("GetRequestID() = %s on error, want the nil UUID", id)
	}
}

// A server with the logging interceptor but not the request ID one in front of
// it is misconfigured, so its calls fail as internal before reaching the
// handler, and are logged at error so the misconfiguration shows.
func TestLoggingInterceptorWithoutRequestID(t *testing.T) {
	var called bool
	var id uuid.UUID
	var idErr error
	status, body, logs, errs, header := serve(t, idPing{&called, &id, &idErr}, nil, false)
	if len(logs) != 1 {
		t.Fatalf("%d log lines, want 1: %v", len(logs), logs)
	}
	entry := logs[0]
	if entry["level"] != "ERROR" {
		t.Errorf("level = %v, want ERROR", entry["level"])
	}
	if entry["msg"] != "request couldn't be processed" {
		t.Errorf("msg = %q, want %q", entry["msg"], "request couldn't be processed")
	}
	if entry["procedure"] != pingv1connect.PingServicePingProcedure {
		t.Errorf("procedure = %v, want %q", entry["procedure"], pingv1connect.PingServicePingProcedure)
	}
	if addr, _ := entry["caller-address"].(string); addr == "" {
		t.Errorf("caller-address = %v, want an address", entry["caller-address"])
	}
	if got, ok := entry["request-id"]; ok {
		t.Errorf("request-id = %v, want none logged", got)
	}
	if len(errs) != 1 {
		t.Fatalf("logged %d errors, want 1: %v", len(errs), errs)
	}
	if _, ok := errs[0].(error); !ok {
		t.Errorf("error = %T %v, want an error", errs[0], errs[0])
	}
	if got := header.Get("X-Request-Id"); got != "" {
		t.Errorf("response X-Request-Id = %q, want none", got)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", status, http.StatusInternalServerError)
	}
	var res struct{ Code string }
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decoding response %q: %v", body, err)
	}
	if res.Code != connect.CodeInternal.String() {
		t.Errorf("code = %q, want %q", res.Code, connect.CodeInternal.String())
	}
	if called {
		t.Errorf("the handler was called, want the call stopped before it")
	}
}

// Only NewRequestIDInterceptor sets the ID: a value stored by another package,
// even under the same name, isn't taken for it.
func TestGetRequestIDIgnoresOtherKeys(t *testing.T) {
	type otherKey string
	want := uuid.NewV7()
	for name, ctx := range map[string]context.Context{
		"string key":        context.WithValue(context.Background(), "request-id", want),
		"other package key": context.WithValue(context.Background(), otherKey("request-id"), want),
	} {
		t.Run(name, func(t *testing.T) {
			if id, err := GetRequestID(ctx); err == nil {
				t.Errorf("GetRequestID() = %s, want an error", id)
			}
		})
	}
}
