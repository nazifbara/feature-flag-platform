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

// call sends a ping through a real server with the interceptor, and returns
// the response's status, its body, and the log lines written meanwhile.
func call(t *testing.T, err error) (status int, body string, logs []map[string]any) {
	t.Helper()
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	server := connect.NewServer(NewLoggingInterceptor())
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
	return res.StatusCode, string(raw), logs
}

func TestLoggingInterceptorLogsFailures(t *testing.T) {
	cause := errors.New(`pq: relation "secret_table" does not exist`)
	const secret = "secret_table"

	tests := map[string]struct {
		err error
		// wantMessage is the logged message.
		wantMessage string
		// wantCause is the logged cause. Empty means none is logged.
		wantCause string
		// wantClient is the message the client gets.
		wantClient string
	}{
		"internal with a cause": {
			err:         connect.NewError(connect.CodeInternal, "can't create the rule").WithCause(cause),
			wantMessage: "internal: can't create the rule",
			wantCause:   cause.Error(),
			wantClient:  "can't create the rule",
		},
		"already exists with a cause": {
			err:         connect.NewError(connect.CodeAlreadyExists, "can't create the rule").WithCause(cause),
			wantMessage: "already_exists: can't create the rule",
			wantCause:   cause.Error(),
			wantClient:  "can't create the rule",
		},
		"not found with a cause": {
			err:         connect.NewError(connect.CodeNotFound, "can't get the rule").WithCause(cause),
			wantMessage: "not_found: can't get the rule",
			wantCause:   cause.Error(),
			wantClient:  "can't get the rule",
		},
		"connect error without a cause": {
			err:         connect.NewError(connect.CodeInvalidArgument, "bad name"),
			wantMessage: "invalid_argument: bad name",
			wantClient:  "bad name",
		},
		// The cause is found through any wrapping.
		"wrapped connect error": {
			err:         fmt.Errorf("signing up: %w", connect.NewError(connect.CodeInternal, "can't sign up").WithCause(cause)),
			wantMessage: "signing up: internal: can't sign up",
			wantCause:   cause.Error(),
			wantClient:  "can't sign up",
		},
		// All of a plain error's text is already in the message, and connect
		// keeps it from the client.
		"plain wrapped error": {
			err:         fmt.Errorf("outer: %w", errors.New("inner")),
			wantMessage: "outer: inner",
			wantClient:  "",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, body, logs := call(t, test.err)
			if len(logs) != 1 {
				t.Fatalf("%d log lines, want 1: %v", len(logs), logs)
			}
			entry := logs[0]
			if entry["level"] != "ERROR" {
				t.Errorf("level = %v, want ERROR", entry["level"])
			}
			if entry["msg"] != test.wantMessage {
				t.Errorf("msg = %q, want %q", entry["msg"], test.wantMessage)
			}
			got, logged := entry["cause"]
			if test.wantCause == "" && logged {
				t.Errorf("cause = %q, want none", got)
			}
			if test.wantCause != "" && got != test.wantCause {
				t.Errorf("cause = %v, want %q", got, test.wantCause)
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

func TestLoggingInterceptorIgnoresSuccess(t *testing.T) {
	status, _, logs := call(t, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if len(logs) != 0 {
		t.Fatalf("logged %v for a successful call, want nothing", logs)
	}
}
