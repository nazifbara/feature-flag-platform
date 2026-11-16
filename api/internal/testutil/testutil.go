// Package testutil holds what the packages testing against TEST_DB_URL share:
// the lock that keeps them from running at once, the database helpers, and the
// checks on connect errors.
package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	_ "github.com/lib/pq"
)

// DBLock is the session advisory lock every package testing against
// TEST_DB_URL holds for its whole run. go test runs packages in parallel, and
// the tests empty the tables and count rows, so they'd break each other.
const DBLock = 7236402

// UnreachableDB is a database URL nothing listens on, for a store whose every
// query fails.
const UnreachableDB = "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"

// Main runs m's tests while holding DBLock, then exits with their code. A
// package's TestMain calls it.
func Main(m *testing.M) {
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
	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_lock($1)", DBLock); err != nil {
		fmt.Fprintf(os.Stderr, "locking the test database: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	// Closing the database ends the session, which releases the lock.
	_ = conn.Close()
	_ = db.Close()
	os.Exit(code)
}

// OpenDB connects to TEST_DB_URL for the test to look at the tables directly,
// and empties them. The connection closes when the test ends.
func OpenDB(t *testing.T) *sql.DB {
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
	ResetDB(t, db)
	return db
}

// ResetDB empties every table except goose's version table.
func ResetDB(t *testing.T, db *sql.DB) {
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
		t.Fatalf("ResetDB: %v", err)
	}
}

// DBNow reads the database's clock, which stamps created_at and updated_at.
// The test's own clock can be off from it, as with Postgres in a Docker VM.
func DBNow(t *testing.T, db *sql.DB) time.Time {
	t.Helper()
	var now time.Time
	if err := db.QueryRow("SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatalf("reading the database's clock: %v", err)
	}
	return now
}

// WantCode fails the test unless err is a *connect.Error with code and, when
// message isn't empty, that message.
func WantCode(t *testing.T, err error, code connect.Code, message string) {
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

// BadIDs are ids a proto field with the uuid rule rejects, with the message the
// client gets when the field is named id. The rule takes only the dashed form,
// unlike uuid.Parse.
var BadIDs = map[string]struct {
	ID      string
	Message string
}{
	"an empty id": {
		ID:      "",
		Message: "validation error: id: value is empty, which is not a valid UUID",
	},
	"an id that isn't a UUID": {
		ID:      "not-a-uuid",
		Message: "validation error: id: must be a valid UUID",
	},
	"an id with surrounding spaces": {
		ID:      " 6ba7b810-9dad-11d1-80b4-00c04fd430c8 ",
		Message: "validation error: id: must be a valid UUID",
	},
	"an id in braces": {
		ID:      "{6ba7b810-9dad-11d1-80b4-00c04fd430c8}",
		Message: "validation error: id: must be a valid UUID",
	},
	"an id as a URN": {
		ID:      "urn:uuid:6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		Message: "validation error: id: must be a valid UUID",
	},
	"an id without dashes": {
		ID:      "6ba7b8109dad11d180b400c04fd430c8",
		Message: "validation error: id: must be a valid UUID",
	},
}

// UnparsableIDs are ids uuid.Parse rejects, for handlers called without the
// validation interceptor.
var UnparsableIDs = []string{"", "not-a-uuid", " 6ba7b810-9dad-11d1-80b4-00c04fd430c8 "}
