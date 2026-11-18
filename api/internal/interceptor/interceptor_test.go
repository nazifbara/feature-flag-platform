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
	var logged bytes.Buffer
	logger := slog.New(slog.NewMultiHandler(slog.NewJSONHandler(&logged, nil), recorder{&errs}))

	server := connect.NewServer(NewLoggingInterceptor(logger))
	pingv1connect.RegisterPingServiceHandler(server, failingPing{err: err})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	res, postErr := http.Post(ts.URL+pingv1connect.PingServicePingProcedure, "application/json", strings.NewReader(`{"message":"hi"}`))
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
		logs = append(logs, entry)
	}
	return res.StatusCode, string(raw), logs, errs
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
			if entry["msg"] != "Served Request" {
				t.Errorf("msg = %q, want %q", entry["msg"], "Served Request")
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
	if entry["msg"] != "Served Request" {
		t.Errorf("msg = %q, want %q", entry["msg"], "Served Request")
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
