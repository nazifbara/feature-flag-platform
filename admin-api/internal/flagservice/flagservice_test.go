package flagservice

import (
	"context"
	"database/sql"
	"encoding/base64"
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
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	flagv1 "github.com/nazifbara/feature-flag-platform/gen/go/flagsvc/flag/v1"
	"github.com/nazifbara/feature-flag-platform/gen/go/flagsvc/flag/v1/flagv1connect"
	"google.golang.org/protobuf/proto"

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
func newClient(t *testing.T, dbURL string) flagv1connect.FlagServiceClient {
	t.Helper()
	st, err := store.New(dbURL)
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}
	server := connect.NewServer(validate.NewServerInterceptor())
	flagv1connect.RegisterFlagServiceHandler(server, New(st))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return flagv1connect.NewFlagServiceClient(
		connect.NewClient(connecthttp.NewTransport(ts.Client(), ts.URL)),
	)
}

// unreachableDB is a database URL nothing listens on, for a store whose every
// query fails.
const unreachableDB = "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"

// badIDs are flag ids the proto's uuid rule rejects, with the message the
// client gets. The rule takes only the dashed form, unlike uuid.Parse.
var badIDs = map[string]struct {
	id      string
	message string
}{
	"an empty id": {
		id:      "",
		message: "validation error: id: value is empty, which is not a valid UUID",
	},
	"an id that isn't a UUID": {
		id:      "not-a-uuid",
		message: "validation error: id: must be a valid UUID",
	},
	"an id with surrounding spaces": {
		id:      " 6ba7b810-9dad-11d1-80b4-00c04fd430c8 ",
		message: "validation error: id: must be a valid UUID",
	},
	"an id in braces": {
		id:      "{6ba7b810-9dad-11d1-80b4-00c04fd430c8}",
		message: "validation error: id: must be a valid UUID",
	},
	"an id as a URN": {
		id:      "urn:uuid:6ba7b810-9dad-11d1-80b4-00c04fd430c8",
		message: "validation error: id: must be a valid UUID",
	},
	"an id without dashes": {
		id:      "6ba7b8109dad11d180b400c04fd430c8",
		message: "validation error: id: must be a valid UUID",
	},
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

// dbNow reads the database's clock, which stamps created_at and updated_at.
// The test's own clock can be off from it, as with Postgres in a Docker VM.
func dbNow(t *testing.T, db *sql.DB) time.Time {
	t.Helper()
	var now time.Time
	if err := db.QueryRow("SELECT clock_timestamp()").Scan(&now); err != nil {
		t.Fatalf("reading the database's clock: %v", err)
	}
	return now
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
func mustCreate(t *testing.T, client flagv1connect.FlagServiceClient, key string) *flagv1.Flag {
	t.Helper()
	res, err := client.Create(context.Background(), &flagv1.CreateRequest{Key: key})
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
		before := dbNow(t, db)
		got := mustCreate(t, client, "new-checkout")
		after := dbNow(t, db)

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
		if created.Before(before) || created.After(after) {
			t.Errorf("created_at = %v, want between %v and %v", created, before, after)
		}
		if !updated.Equal(created) {
			t.Errorf("updated_at = %v, want created_at %v", updated, created)
		}
		if got.Archived {
			t.Error("archived = true, want a new flag not archived")
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
			_, err := client.Create(ctx, &flagv1.CreateRequest{Key: tc.key})
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
		_, err := client.Create(ctx, &flagv1.CreateRequest{Key: "New-Checkout"})
		wantCode(t, err, connect.CodeAlreadyExists, "can't create the flag")
		if n := flagCount(t, db); n != 1 {
			t.Fatalf("%d flags after the duplicate, want 1", n)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Create(ctx, &flagv1.CreateRequest{Key: "new-checkout"})
		wantCode(t, err, connect.CodeInternal, "can't create the flag")
	})
}

// flagRow is a flag as the flags table holds it.
type flagRow struct {
	key, salt        string
	created, updated time.Time
}

// readFlag reads the flag with id from db.
func readFlag(t *testing.T, db *sql.DB, id string) flagRow {
	t.Helper()
	var r flagRow
	err := db.QueryRow("SELECT key, salt, created_at, updated_at FROM flags WHERE id = $1", id).
		Scan(&r.key, &r.salt, &r.created, &r.updated)
	if err != nil {
		t.Fatalf("reading flag %s: %v", id, err)
	}
	return r
}

// mustUpdate gives the flag with id the key and returns the response's flag,
// failing the test if the call fails or the response carries no flag.
func mustUpdate(t *testing.T, client flagv1connect.FlagServiceClient, id, key string) *flagv1.Flag {
	t.Helper()
	res, err := client.Update(context.Background(), &flagv1.UpdateRequest{Id: id, Key: key})
	if err != nil {
		t.Fatalf("Update(%s, %q): %v", id, key, err)
	}
	if res.GetFlag() == nil {
		t.Fatalf("Update(%s, %q) returned no flag", id, key)
	}
	return res.GetFlag()
}

func TestUpdate(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("returns the renamed flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "old-checkout")
		got := mustUpdate(t, client, created.Id, "new-checkout")

		if got.Id != created.Id || got.Salt != created.Salt {
			t.Errorf("id, salt = %s, %q, want the flag's %s, %q", got.Id, got.Salt, created.Id, created.Salt)
		}
		if got.Key != "new-checkout" {
			t.Errorf("key = %q, want %q", got.Key, "new-checkout")
		}
		if !got.GetCreatedAt().AsTime().Equal(created.GetCreatedAt().AsTime()) {
			t.Errorf("created_at = %v, want %v unchanged", got.GetCreatedAt().AsTime(), created.GetCreatedAt().AsTime())
		}
		if !got.GetUpdatedAt().AsTime().After(created.GetUpdatedAt().AsTime()) {
			t.Errorf("updated_at = %v, want after %v", got.GetUpdatedAt().AsTime(), created.GetUpdatedAt().AsTime())
		}

		row := readFlag(t, db, created.Id)
		if row.key != got.Key || row.salt != got.Salt || !row.created.Equal(got.GetCreatedAt().AsTime()) || !row.updated.Equal(got.GetUpdatedAt().AsTime()) {
			t.Errorf("stored flag = %+v, want the response's %q, %q, %v, %v",
				row, got.Key, got.Salt, got.GetCreatedAt().AsTime(), got.GetUpdatedAt().AsTime())
		}
	})

	// The response carries the key as stored, not as sent.
	t.Run("returns the normalized key", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "old-checkout")
		got := mustUpdate(t, client, created.Id, "  New-Checkout  ")
		if got.Key != "new-checkout" {
			t.Fatalf("key = %q, want %q", got.Key, "new-checkout")
		}
	})

	// A key that normalizes to the current one changes nothing, not even
	// updated_at.
	t.Run("leaves the flag alone when the key is the same", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		got := mustUpdate(t, client, created.Id, "New-Checkout")
		if got.Key != "new-checkout" {
			t.Errorf("key = %q, want %q", got.Key, "new-checkout")
		}
		if !got.GetUpdatedAt().AsTime().Equal(created.GetUpdatedAt().AsTime()) {
			t.Errorf("updated_at = %v, want %v unchanged", got.GetUpdatedAt().AsTime(), created.GetUpdatedAt().AsTime())
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "old-checkout")
		got := mustUpdate(t, client, strings.ToUpper(created.Id), "new-checkout")
		if got.Id != created.Id {
			t.Fatalf("id = %s, want %s", got.Id, created.Id)
		}
	})

	// Rejected ids and keys leave the flag as it was, whether the proto
	// rules or the store catch them.
	for name, tc := range badIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			created := mustCreate(t, client, "old-checkout")
			before := readFlag(t, db, created.Id)
			_, err := client.Update(ctx, &flagv1.UpdateRequest{Id: tc.id, Key: "new-checkout"})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
			if after := readFlag(t, db, created.Id); after != before {
				t.Fatalf("flag = %+v after a rejected update, want %+v unchanged", after, before)
			}
		})
	}
	rejected := map[string]struct {
		// id replaces the flag's id when it isn't empty.
		id      string
		key     string
		code    connect.Code
		message string
	}{
		"an id no flag has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			key:     "new-checkout",
			code:    connect.CodeNotFound,
			message: "can't update the flag",
		},
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
			message: "can't update a flag with an empty key",
		},
		"a key with a space inside": {
			key:     "new checkout",
			code:    connect.CodeInvalidArgument,
			message: "can't update a flag with whitespace in its key",
		},
		"a key with a character outside the allowed set": {
			key:     "new_checkout",
			code:    connect.CodeInvalidArgument,
			message: "can't update a flag with '_' (U+005F) in its key: only letters, digits and - are allowed",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			created := mustCreate(t, client, "old-checkout")
			before := readFlag(t, db, created.Id)
			id := created.Id
			if tc.id != "" {
				id = tc.id
			}
			_, err := client.Update(ctx, &flagv1.UpdateRequest{Id: id, Key: tc.key})
			wantCode(t, err, tc.code, tc.message)
			if after := readFlag(t, db, created.Id); after != before {
				t.Fatalf("flag = %+v after a rejected update, want %+v unchanged", after, before)
			}
		})
	}

	t.Run("accepts a key of exactly 100 characters", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "old-checkout")
		key := strings.Repeat("a", 100)
		if got := mustUpdate(t, client, created.Id, key); got.Key != key {
			t.Fatalf("key = %q, want %q", got.Key, key)
		}
	})

	// Keys are compared after normalizing, so case doesn't dodge a taken key.
	t.Run("rejects a key another flag has", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "new-checkout")
		other := mustCreate(t, client, "old-checkout")
		before := readFlag(t, db, other.Id)
		_, err := client.Update(ctx, &flagv1.UpdateRequest{Id: other.Id, Key: "New-Checkout"})
		wantCode(t, err, connect.CodeAlreadyExists, "can't update the flag")
		if after := readFlag(t, db, other.Id); after != before {
			t.Fatalf("flag = %+v after a rejected update, want %+v unchanged", after, before)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Update(ctx, &flagv1.UpdateRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Key: "new-checkout"})
		wantCode(t, err, connect.CodeInternal, "can't update the flag")
	})
}

// mustGet gets the flag with id and returns the response's flag, failing the
// test if the call fails or the response carries no flag.
func mustGet(t *testing.T, client flagv1connect.FlagServiceClient, id string) *flagv1.Flag {
	t.Helper()
	res, err := client.Get(context.Background(), &flagv1.GetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if res.GetFlag() == nil {
		t.Fatalf("Get(%s) returned no flag", id)
	}
	return res.GetFlag()
}

func TestGet(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("returns the stored flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		got := mustGet(t, client, created.Id)
		if !proto.Equal(got, created) {
			t.Fatalf("Get() = %v, want the created flag %v", got, created)
		}
	})

	t.Run("returns the flag as last updated", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "old-checkout")
		updated := mustUpdate(t, client, created.Id, "new-checkout")
		got := mustGet(t, client, created.Id)
		if !proto.Equal(got, updated) {
			t.Fatalf("Get() = %v, want the updated flag %v", got, updated)
		}
	})

	t.Run("returns the flag asked for among others", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "first")
		second := mustCreate(t, client, "second")
		mustCreate(t, client, "third")
		if got := mustGet(t, client, second.Id); !proto.Equal(got, second) {
			t.Fatalf("Get() = %v, want %v", got, second)
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		if got := mustGet(t, client, strings.ToUpper(created.Id)); got.Id != created.Id {
			t.Fatalf("id = %s, want %s", got.Id, created.Id)
		}
	})

	for name, tc := range badIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.Get(ctx, &flagv1.GetRequest{Id: tc.id})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
		})
	}

	t.Run("rejects an id no flag has", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "new-checkout")
		_, err := client.Get(ctx, &flagv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeNotFound, "can't get the flag")
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Get(ctx, &flagv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeInternal, "can't get the flag")
	})
}

// mustDelete deletes the flag with id and returns the response's flag, failing
// the test if the call fails or the response carries no flag.
func mustDelete(t *testing.T, client flagv1connect.FlagServiceClient, id string) *flagv1.Flag {
	t.Helper()
	res, err := client.Delete(context.Background(), &flagv1.DeleteRequest{Id: id})
	if err != nil {
		t.Fatalf("Delete(%s): %v", id, err)
	}
	if res.GetFlag() == nil {
		t.Fatalf("Delete(%s) returned no flag", id)
	}
	return res.GetFlag()
}

// flagExists reports whether db holds a flag with id.
func flagExists(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var exists bool
	if err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM flags WHERE id = $1)", id).Scan(&exists); err != nil {
		t.Fatalf("looking for flag %s: %v", id, err)
	}
	return exists
}

func TestDelete(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("returns the flag as it was", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "old-checkout")
		updated := mustUpdate(t, client, created.Id, "new-checkout")
		if got := mustDelete(t, client, created.Id); !proto.Equal(got, updated) {
			t.Fatalf("Delete() = %v, want the flag as last updated %v", got, updated)
		}
	})

	t.Run("removes the flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustDelete(t, client, created.Id)
		if flagExists(t, db, created.Id) {
			t.Fatal("the flag is still stored after Delete()")
		}
		_, err := client.Get(ctx, &flagv1.GetRequest{Id: created.Id})
		wantCode(t, err, connect.CodeNotFound, "can't get the flag")
	})

	t.Run("removes only the flag asked for", func(t *testing.T) {
		resetDB(t, db)
		first := mustCreate(t, client, "first")
		second := mustCreate(t, client, "second")
		third := mustCreate(t, client, "third")
		mustDelete(t, client, second.Id)
		for _, f := range []*flagv1.Flag{first, third} {
			if got := mustGet(t, client, f.Id); !proto.Equal(got, f) {
				t.Errorf("Get(%s) = %v, want %v unchanged", f.Key, got, f)
			}
		}
	})

	t.Run("rejects deleting the same flag twice", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustDelete(t, client, created.Id)
		_, err := client.Delete(ctx, &flagv1.DeleteRequest{Id: created.Id})
		wantCode(t, err, connect.CodeNotFound, "can't delete the flag")
	})

	// A new flag can take a deleted flag's key, and is a flag of its own.
	t.Run("frees the key", func(t *testing.T) {
		resetDB(t, db)
		deleted := mustCreate(t, client, "new-checkout")
		mustDelete(t, client, deleted.Id)
		reused := mustCreate(t, client, "new-checkout")
		if reused.Id == deleted.Id || reused.Salt == deleted.Salt {
			t.Fatalf("new flag id, salt = %s, %q, want neither of the deleted flag's %s, %q",
				reused.Id, reused.Salt, deleted.Id, deleted.Salt)
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		if got := mustDelete(t, client, strings.ToUpper(created.Id)); got.Id != created.Id {
			t.Fatalf("id = %s, want %s", got.Id, created.Id)
		}
	})

	// Rejected ids delete nothing.
	for name, tc := range badIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			created := mustCreate(t, client, "new-checkout")
			_, err := client.Delete(ctx, &flagv1.DeleteRequest{Id: tc.id})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
			if !flagExists(t, db, created.Id) {
				t.Fatal("the flag is gone after a rejected delete")
			}
		})
	}

	t.Run("rejects an id no flag has", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		_, err := client.Delete(ctx, &flagv1.DeleteRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeNotFound, "can't delete the flag")
		if !flagExists(t, db, created.Id) {
			t.Fatal("another flag is gone after deleting an id no flag has")
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Delete(ctx, &flagv1.DeleteRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeInternal, "can't delete the flag")
	})
}

// mustList lists the first page, failing the test if the call fails or there's
// a next page: callers have fewer flags than fill one.
func mustList(t *testing.T, client flagv1connect.FlagServiceClient) []*flagv1.Flag {
	t.Helper()
	res := listPage(t, client, nil)
	if res.NextPageToken != nil {
		t.Fatalf("List() next_page_token = %q with %d flags, want none", *res.NextPageToken, len(res.GetFlags()))
	}
	return res.GetFlags()
}

// listPage lists the page token points to, or the first page for a nil token,
// failing the test if the call fails.
func listPage(t *testing.T, client flagv1connect.FlagServiceClient, token *string) *flagv1.ListResponse {
	t.Helper()
	req := &flagv1.ListRequest{PageToken: token}
	res, err := client.List(context.Background(), req)
	if err != nil {
		t.Fatalf("List(%q): %v", req.GetPageToken(), err)
	}
	return res
}

// keys gives the keys of flags, in order, to report a list compactly.
func keys(flags []*flagv1.Flag) []string {
	keys := make([]string, len(flags))
	for i, f := range flags {
		keys[i] = f.GetKey()
	}
	return keys
}

// wantFlags fails the test unless got holds want's flags in want's order.
func wantFlags(t *testing.T, got, want []*flagv1.Flag) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("List() = %d flags %v, want %d %v", len(got), keys(got), len(want), keys(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Fatalf("List()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestList(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("no flags", func(t *testing.T) {
		resetDB(t, db)
		wantFlags(t, mustList(t, client), nil)
	})

	// Each flag is the one Create returned, in key order rather than the
	// order they were made.
	t.Run("returns every flag ordered by key", func(t *testing.T) {
		resetDB(t, db)
		c := mustCreate(t, client, "c")
		a := mustCreate(t, client, "a")
		b := mustCreate(t, client, "b")
		wantFlags(t, mustList(t, client), []*flagv1.Flag{a, b, c})
	})

	t.Run("returns flags as last updated", func(t *testing.T) {
		resetDB(t, db)
		a := mustCreate(t, client, "a")
		b := mustCreate(t, client, "b")
		renamed := mustUpdate(t, client, a.Id, "c")
		wantFlags(t, mustList(t, client), []*flagv1.Flag{b, renamed})
	})

	t.Run("leaves out deleted flags", func(t *testing.T) {
		resetDB(t, db)
		a := mustCreate(t, client, "a")
		b := mustCreate(t, client, "b")
		c := mustCreate(t, client, "c")
		mustDelete(t, client, b.Id)
		wantFlags(t, mustList(t, client), []*flagv1.Flag{a, c})
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.List(ctx, &flagv1.ListRequest{})
		wantCode(t, err, connect.CodeInternal, "can't list flags")
	})
}

// mustCreateMany creates n flags keyed flag-000, flag-001 and so on, which sort
// as they're numbered, and returns them in that order.
func mustCreateMany(t *testing.T, client flagv1connect.FlagServiceClient, n int) []*flagv1.Flag {
	t.Helper()
	flags := make([]*flagv1.Flag, n)
	for i := range flags {
		flags[i] = mustCreate(t, client, fmt.Sprintf("flag-%03d", i))
	}
	return flags
}

func TestListPages(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("a full page has no next page token", func(t *testing.T) {
		resetDB(t, db)
		flags := mustCreateMany(t, client, store.MaxFlagsPerPage)
		wantFlags(t, mustList(t, client), flags)
	})

	t.Run("one flag past a page gives a second page", func(t *testing.T) {
		resetDB(t, db)
		flags := mustCreateMany(t, client, store.MaxFlagsPerPage+1)
		first := listPage(t, client, nil)
		wantFlags(t, first.GetFlags(), flags[:store.MaxFlagsPerPage])
		if first.NextPageToken == nil || *first.NextPageToken == "" {
			t.Fatalf("first page next_page_token = %v, want a token", first.NextPageToken)
		}
		second := listPage(t, client, first.NextPageToken)
		wantFlags(t, second.GetFlags(), flags[store.MaxFlagsPerPage:])
		if second.NextPageToken != nil {
			t.Fatalf("second page next_page_token = %q, want none", *second.NextPageToken)
		}
	})

	t.Run("following tokens lists every flag once", func(t *testing.T) {
		resetDB(t, db)
		flags := mustCreateMany(t, client, 2*store.MaxFlagsPerPage+7)
		var got []*flagv1.Flag
		var token *string
		for pages := 1; ; pages++ {
			if pages > 4 {
				t.Fatalf("still listing after %d pages", pages-1)
			}
			res := listPage(t, client, token)
			if len(res.GetFlags()) > store.MaxFlagsPerPage {
				t.Fatalf("page %d has %d flags, want at most %d", pages, len(res.GetFlags()), store.MaxFlagsPerPage)
			}
			got = append(got, res.GetFlags()...)
			if token = res.NextPageToken; token == nil {
				break
			}
		}
		wantFlags(t, got, flags)
	})

	// The token's flag can be deleted between pages; the next page still
	// starts at the first key after it.
	t.Run("a token whose flag is gone still works", func(t *testing.T) {
		resetDB(t, db)
		flags := mustCreateMany(t, client, store.MaxFlagsPerPage+2)
		first := listPage(t, client, nil)
		if first.NextPageToken == nil {
			t.Fatal("first page has no next_page_token")
		}
		last := first.GetFlags()[len(first.GetFlags())-1]
		mustDelete(t, client, last.Id)
		wantFlags(t, listPage(t, client, first.NextPageToken).GetFlags(), flags[store.MaxFlagsPerPage:])
	})

	t.Run("rejects an empty page token", func(t *testing.T) {
		empty := ""
		_, err := client.List(ctx, &flagv1.ListRequest{PageToken: &empty})
		wantCode(t, err, connect.CodeInvalidArgument, "validation error: page_token: must be at least 1 characters")
	})

	for name, token := range map[string]string{
		"not base64":              "!!!",
		"base64 of non-JSON":      base64.RawURLEncoding.EncodeToString([]byte("not json")),
		"a key of the wrong type": base64.RawURLEncoding.EncodeToString([]byte(`{"key":1}`)),
	} {
		t.Run("rejects a page token that's "+name, func(t *testing.T) {
			_, err := client.List(ctx, &flagv1.ListRequest{PageToken: &token})
			wantCode(t, err, connect.CodeInvalidArgument, "invalid page cursor")
		})
	}

	// The token is checked before the store is reached, which would answer
	// Internal here.
	t.Run("rejects a bad token before reaching the database", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		token := "!!!"
		_, err := unreachable.List(ctx, &flagv1.ListRequest{PageToken: &token})
		wantCode(t, err, connect.CodeInvalidArgument, "invalid page cursor")
	})

	t.Run("hides database failures on a later page", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		token := base64.RawURLEncoding.EncodeToString([]byte(`{"key":"a"}`))
		_, err := unreachable.List(ctx, &flagv1.ListRequest{PageToken: &token})
		wantCode(t, err, connect.CodeInternal, "can't list flags")
	})
}

// Without the validation interceptor, as for a direct caller, an empty page
// token is still rejected rather than read as the first page, before reaching
// the store: it can't reach a database, and would answer Internal.
func TestListWithoutValidationRejectsEmptyToken(t *testing.T) {
	st, err := store.New(unreachableDB)
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}
	empty := ""
	_, err = New(st).List(context.Background(), &flagv1.ListRequest{PageToken: &empty})
	wantCode(t, err, connect.CodeInvalidArgument, "invalid page cursor")
}

// Without the validation interceptor, as for a direct caller, each handler's
// own parsing still rejects an id that isn't a UUID, before reaching the
// store: the store here can't reach a database, and would answer Internal.
func TestHandlersWithoutValidationRejectBadIDs(t *testing.T) {
	st, err := store.New(unreachableDB)
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}
	svc := New(st)
	ctx := context.Background()
	calls := map[string]func(id string) error{
		"Update": func(id string) error {
			_, err := svc.Update(ctx, &flagv1.UpdateRequest{Id: id, Key: "new-checkout"})
			return err
		},
		"Get": func(id string) error {
			_, err := svc.Get(ctx, &flagv1.GetRequest{Id: id})
			return err
		},
		"Delete": func(id string) error {
			_, err := svc.Delete(ctx, &flagv1.DeleteRequest{Id: id})
			return err
		},
		"Archive": func(id string) error {
			_, err := svc.Archive(ctx, &flagv1.ArchiveRequest{Id: id})
			return err
		},
		"Unarchive": func(id string) error {
			_, err := svc.Unarchive(ctx, &flagv1.UnarchiveRequest{Id: id})
			return err
		},
	}
	for rpc, call := range calls {
		for _, id := range []string{"", "not-a-uuid", " 6ba7b810-9dad-11d1-80b4-00c04fd430c8 "} {
			t.Run(fmt.Sprintf("%s %q", rpc, id), func(t *testing.T) {
				wantCode(t, call(id), connect.CodeInvalidArgument, "invalid flag id")
			})
		}
	}
}

// isArchived reports whether the flag with id is archived in db, for checking
// what was stored rather than what a response says.
func isArchived(t *testing.T, db *sql.DB, id string) bool {
	t.Helper()
	var archived bool
	if err := db.QueryRow("SELECT archived FROM flags WHERE id = $1", id).Scan(&archived); err != nil {
		t.Fatalf("reading whether flag %s is archived: %v", id, err)
	}
	return archived
}

// mustArchive archives the flag with id and returns the response's flag,
// failing the test if the call fails or the response carries no flag.
func mustArchive(t *testing.T, client flagv1connect.FlagServiceClient, id string) *flagv1.Flag {
	t.Helper()
	res, err := client.Archive(context.Background(), &flagv1.ArchiveRequest{Id: id})
	if err != nil {
		t.Fatalf("Archive(%s): %v", id, err)
	}
	if res.GetFlag() == nil {
		t.Fatalf("Archive(%s) returned no flag", id)
	}
	return res.GetFlag()
}

// mustUnarchive unarchives the flag with id and returns the response's flag,
// failing the test if the call fails or the response carries no flag.
func mustUnarchive(t *testing.T, client flagv1connect.FlagServiceClient, id string) *flagv1.Flag {
	t.Helper()
	res, err := client.Unarchive(context.Background(), &flagv1.UnarchiveRequest{Id: id})
	if err != nil {
		t.Fatalf("Unarchive(%s): %v", id, err)
	}
	if res.GetFlag() == nil {
		t.Fatalf("Unarchive(%s) returned no flag", id)
	}
	return res.GetFlag()
}

func TestArchive(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("archives the flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, created.Id)
		if !isArchived(t, db, created.Id) {
			t.Fatal("the flag isn't archived after Archive()")
		}
	})

	// Only archived moves: Get shows the flag as Archive returned it.
	t.Run("returns the archived flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		want := proto.Clone(created).(*flagv1.Flag)
		want.Archived = true
		if got := mustArchive(t, client, created.Id); !proto.Equal(got, want) {
			t.Fatalf("Archive() = %v, want %v", got, want)
		}
		if got := mustGet(t, client, created.Id); !proto.Equal(got, want) {
			t.Fatalf("Get() = %v, want %v", got, want)
		}
	})

	t.Run("returns an archived flag as it is", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		first := mustArchive(t, client, created.Id)
		if got := mustArchive(t, client, created.Id); !proto.Equal(got, first) {
			t.Fatalf("Archive() = %v, want %v", got, first)
		}
	})

	// Archived flags stay listed, marked as archived.
	t.Run("lists the flag as archived", func(t *testing.T) {
		resetDB(t, db)
		first := mustCreate(t, client, "first")
		second := mustArchive(t, client, mustCreate(t, client, "second").Id)
		wantFlags(t, mustList(t, client), []*flagv1.Flag{first, second})
	})

	t.Run("archives only the flag asked for", func(t *testing.T) {
		resetDB(t, db)
		first := mustCreate(t, client, "first")
		second := mustCreate(t, client, "second")
		third := mustCreate(t, client, "third")
		mustArchive(t, client, second.Id)
		for _, f := range []*flagv1.Flag{first, third} {
			if isArchived(t, db, f.Id) {
				t.Errorf("%s is archived after archiving %s", f.Key, second.Key)
			}
		}
	})

	t.Run("accepts an archived flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, created.Id)
		mustArchive(t, client, created.Id)
		if !isArchived(t, db, created.Id) {
			t.Fatal("the flag isn't archived after archiving it twice")
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, strings.ToUpper(created.Id))
		if !isArchived(t, db, created.Id) {
			t.Fatal("the flag isn't archived after Archive() with its id in upper case")
		}
	})

	// Rejected ids archive nothing.
	for name, tc := range badIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			created := mustCreate(t, client, "new-checkout")
			_, err := client.Archive(ctx, &flagv1.ArchiveRequest{Id: tc.id})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
			if isArchived(t, db, created.Id) {
				t.Fatal("the flag is archived after a rejected archive")
			}
		})
	}

	t.Run("rejects an id no flag has", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		_, err := client.Archive(ctx, &flagv1.ArchiveRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeNotFound, "can't archive flag")
		if isArchived(t, db, created.Id) {
			t.Fatal("another flag is archived after archiving an id no flag has")
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Archive(ctx, &flagv1.ArchiveRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeInternal, "can't archive flag")
	})
}

func TestUnarchive(t *testing.T) {
	db := testDB(t)
	client := newClient(t, os.Getenv("TEST_DB_URL"))
	ctx := context.Background()

	t.Run("unarchives the flag", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, created.Id)
		mustUnarchive(t, client, created.Id)
		if isArchived(t, db, created.Id) {
			t.Fatal("the flag is still archived after Unarchive()")
		}
	})

	// Unarchiving gives back the flag as it was before it was archived.
	t.Run("returns the flag as it was", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, created.Id)
		if got := mustUnarchive(t, client, created.Id); !proto.Equal(got, created) {
			t.Fatalf("Unarchive() = %v, want %v", got, created)
		}
		if got := mustGet(t, client, created.Id); !proto.Equal(got, created) {
			t.Fatalf("Get() = %v, want %v unchanged", got, created)
		}
	})

	t.Run("returns a flag that isn't archived as it is", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		if got := mustUnarchive(t, client, created.Id); !proto.Equal(got, created) {
			t.Fatalf("Unarchive() = %v, want %v", got, created)
		}
	})

	t.Run("unarchives only the flag asked for", func(t *testing.T) {
		resetDB(t, db)
		first := mustCreate(t, client, "first")
		second := mustCreate(t, client, "second")
		third := mustCreate(t, client, "third")
		for _, f := range []*flagv1.Flag{first, second, third} {
			mustArchive(t, client, f.Id)
		}
		mustUnarchive(t, client, second.Id)
		for _, f := range []*flagv1.Flag{first, third} {
			if !isArchived(t, db, f.Id) {
				t.Errorf("%s isn't archived after unarchiving %s", f.Key, second.Key)
			}
		}
	})

	// A flag is created unarchived, so this unarchives a flag that never was
	// archived.
	t.Run("accepts a flag that isn't archived", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustUnarchive(t, client, created.Id)
		if isArchived(t, db, created.Id) {
			t.Fatal("the flag is archived after unarchiving it")
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, created.Id)
		mustUnarchive(t, client, strings.ToUpper(created.Id))
		if isArchived(t, db, created.Id) {
			t.Fatal("the flag is still archived after Unarchive() with its id in upper case")
		}
	})

	// Rejected ids unarchive nothing.
	for name, tc := range badIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			created := mustCreate(t, client, "new-checkout")
			mustArchive(t, client, created.Id)
			_, err := client.Unarchive(ctx, &flagv1.UnarchiveRequest{Id: tc.id})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
			if !isArchived(t, db, created.Id) {
				t.Fatal("the flag is unarchived after a rejected unarchive")
			}
		})
	}

	t.Run("rejects an id no flag has", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "new-checkout")
		mustArchive(t, client, created.Id)
		_, err := client.Unarchive(ctx, &flagv1.UnarchiveRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeNotFound, "can't unarchive flag")
		if !isArchived(t, db, created.Id) {
			t.Fatal("another flag is unarchived after unarchiving an id no flag has")
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Unarchive(ctx, &flagv1.UnarchiveRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeInternal, "can't unarchive flag")
	})
}
