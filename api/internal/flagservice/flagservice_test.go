package flagservice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/validate"
	"github.com/nazifbara/flagsvc/api/internal/store"
	flag "github.com/nazifbara/flagsvc/gen/go/flagsvc/flag/v1"
	"github.com/nazifbara/flagsvc/gen/go/flagsvc/flag/v1/flagconnect"

	_ "github.com/lib/pq"
)

// testDBLock is the session advisory lock every package testing against
// TEST_DB_URL holds for its whole run. go test runs packages in parallel, and
// these tests empty the tables and count rows, so they'd break each other.
// The store tests hold the same one.
const testDBLock = 7236402

// TestMain runs the tests while holding testDBLock.
func TestMain(m *testing.M) {
	db, err := sql.Open("postgres", os.Getenv("TEST_DB_URL"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "opening the test database: %v\n", err)
		os.Exit(1)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "connecting to the test database: %v\n", err)
		os.Exit(1)
	}
	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", testDBLock); err != nil {
		fmt.Fprintf(os.Stderr, "locking the test database: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	// Closing the database ends the session, which releases the lock.
	_ = conn.Close()
	_ = db.Close()
	os.Exit(code)
}

// testDB connects to TEST_DB_URL for the test to look at the tables directly,
// and empties them.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dbURL := os.Getenv("TEST_DB_URL")
	if dbURL == "" {
		t.Fatal("TEST_DB_URL not set")
	}
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		t.Fatalf("opening the test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	resetDB(t, db)
	return db
}

// resetDB empties every table except goose's version table.
func resetDB(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`
		DO $$ DECLARE r record; BEGIN
			FOR r IN SELECT tablename FROM pg_tables
				WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
			LOOP
				EXECUTE 'TRUNCATE TABLE ' || quote_ident(r.tablename) || ' RESTART IDENTITY CASCADE';
			END LOOP;
		END $$;`)
	if err != nil {
		t.Fatalf("resetDB: %v", err)
	}
}

// newClient serves a Service backed by the store at dbURL through a real
// connect server, with the validation interceptor the server runs, and returns
// a client for it.
func newClient(t *testing.T, dbURL string) flagconnect.FlagServiceClient {
	t.Helper()
	st, err := store.New(dbURL)
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}
	server := connect.NewServer(validate.NewServerInterceptor())
	flagconnect.RegisterFlagServiceHandler(server, New(st))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return flagconnect.NewFlagServiceClient(
		connect.NewClient(connecthttp.NewTransport(ts.Client(), ts.URL)),
	)
}

// flagCount gives the number of flags in db.
func flagCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM flags").Scan(&n); err != nil {
		t.Fatalf("counting flags: %v", err)
	}
	return n
}

// wantCode fails the test unless err is a *connect.Error with code and, when
// message isn't empty, that message.
func wantCode(t *testing.T, err error, code connect.Code, message string) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want code %v", code)
	}
	got, ok := errors.AsType[*connect.Error](err)
	if !ok {
		t.Fatalf("error = %v (%T), want *connect.Error", err, err)
	}
	if got.Code() != code {
		t.Fatalf("code = %v (%q), want %v", got.Code(), got.Message(), code)
	}
	if message != "" && got.Message() != message {
		t.Fatalf("message = %q, want %q", got.Message(), message)
	}
}

// mustCreate creates a flag with key and returns the response's flag, failing
// the test if the call fails or the response carries no flag.
func mustCreate(t *testing.T, client flagconnect.FlagServiceClient, key string) *flag.Flag {
	t.Helper()
	res, err := client.Create(context.Background(), &flag.CreateRequest{Key: key})
	if err != nil {
		t.Fatalf("Create(%q): %v", key, err)
	}
	if res.GetFlag() == nil {
		t.Fatalf("Create(%q) returned no flag", key)
	}
	return res.GetFlag()
}

func TestCreate(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("returns the stored flag", func(t *testing.T) {
		resetDB(t, db)
		before := time.Now()
		got := mustCreate(t, client, "new-checkout")
		after := time.Now()

		id, err := uuid.Parse(got.Id)
		if err != nil {
			t.Fatalf("id %q isn't a UUID: %v", got.Id, err)
		}
		if got.Key != "new-checkout" {
			t.Errorf("key = %q, want %q", got.Key, "new-checkout")
		}
		if len(got.Salt) != 32 {
			t.Errorf("salt = %q, want 32 hex characters", got.Salt)
		}
		created, updated := got.GetCreatedAt().AsTime(), got.GetUpdatedAt().AsTime()
		// Postgres keeps microseconds, so the bounds are a microsecond wide.
		if created.Before(before.Truncate(time.Microsecond)) || created.After(after) {
			t.Errorf("created_at = %v, want between %v and %v", created, before, after)
		}
		if !updated.Equal(created) {
			t.Errorf("updated_at = %v, want created_at %v", updated, created)
		}

		var key, salt string
		var storedCreated, storedUpdated time.Time
		err = db.QueryRow("SELECT key, salt, created_at, updated_at FROM flags WHERE id = $1", id).
			Scan(&key, &salt, &storedCreated, &storedUpdated)
		if err != nil {
			t.Fatalf("reading the flag back: %v", err)
		}
		if key != got.Key || salt != got.Salt || !storedCreated.Equal(created) || !storedUpdated.Equal(updated) {
			t.Errorf("stored flag = %q, %q, %v, %v, want the response's %q, %q, %v, %v",
				key, salt, storedCreated, storedUpdated, got.Key, got.Salt, created, updated)
		}
	})

	t.Run("gives each flag its own id and salt", func(t *testing.T) {
		resetDB(t, db)
		first := mustCreate(t, client, "first")
		second := mustCreate(t, client, "second")
		if first.Id == second.Id {
			t.Errorf("both flags got id %q", first.Id)
		}
		if first.Salt == second.Salt {
			t.Errorf("both flags got salt %q", first.Salt)
		}
	})

	// The response carries the key as stored, not as sent.
	t.Run("returns the normalized key", func(t *testing.T) {
		resetDB(t, db)
		got := mustCreate(t, client, "  New-Checkout  ")
		if got.Key != "new-checkout" {
			t.Fatalf("key = %q, want %q", got.Key, "new-checkout")
		}
	})

	// Rejected keys write nothing, whether the proto rules or the store
	// catch them.
	rejected := map[string]struct {
		key     string
		code    connect.Code
		message string
	}{
		// The proto's min_len.
		"an empty key": {
			key:  "",
			code: connect.CodeInvalidArgument,
		},
		// The proto's max_len.
		"a key over 100 characters": {
			key:  strings.Repeat("a", 101),
			code: connect.CodeInvalidArgument,
		},
		// Passes min_len, so the store's check catches it.
		"a key of only spaces": {
			key:     "   ",
			code:    connect.CodeInvalidArgument,
			message: "can't create a flag with an empty key",
		},
		"a key with a space inside": {
			key:     "new checkout",
			code:    connect.CodeInvalidArgument,
			message: "can't create a flag with whitespace in its key",
		},
		"a key with a character outside the allowed set": {
			key:     "new_checkout",
			code:    connect.CodeInvalidArgument,
			message: "can't create a flag with '_' (U+005F) in its key: only letters, digits and - are allowed",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			_, err := client.Create(ctx, &flag.CreateRequest{Key: tc.key})
			wantCode(t, err, tc.code, tc.message)
			if n := flagCount(t, db); n != 0 {
				t.Fatalf("%d flags after a rejected create, want 0", n)
			}
		})
	}

	t.Run("accepts a key of exactly 100 characters", func(t *testing.T) {
		resetDB(t, db)
		key := strings.Repeat("a", 100)
		got := mustCreate(t, client, key)
		if got.Key != key {
			t.Fatalf("key = %q, want %q", got.Key, key)
		}
	})

	// Keys are compared after normalizing, so case doesn't make a new flag.
	t.Run("rejects a key already taken", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "new-checkout")
		_, err := client.Create(ctx, &flag.CreateRequest{Key: "New-Checkout"})
		wantCode(t, err, connect.CodeAlreadyExists, "can't create the flag")
		if n := flagCount(t, db); n != 1 {
			t.Fatalf("%d flags after the duplicate, want 1", n)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2")
		_, err := unreachable.Create(ctx, &flag.CreateRequest{Key: "new-checkout"})
		wantCode(t, err, connect.CodeInternal, "can't create the flag")
	})
}
