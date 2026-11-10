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
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Create(ctx, &flag.CreateRequest{Key: "new-checkout"})
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
func mustUpdate(t *testing.T, client flagconnect.FlagServiceClient, id, key string) *flag.Flag {
	t.Helper()
	res, err := client.Update(context.Background(), &flag.UpdateRequest{Id: id, Key: key})
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
			_, err := client.Update(ctx, &flag.UpdateRequest{Id: tc.id, Key: "new-checkout"})
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
			_, err := client.Update(ctx, &flag.UpdateRequest{Id: id, Key: tc.key})
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
		_, err := client.Update(ctx, &flag.UpdateRequest{Id: other.Id, Key: "New-Checkout"})
		wantCode(t, err, connect.CodeAlreadyExists, "can't update the flag")
		if after := readFlag(t, db, other.Id); after != before {
			t.Fatalf("flag = %+v after a rejected update, want %+v unchanged", after, before)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Update(ctx, &flag.UpdateRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Key: "new-checkout"})
		wantCode(t, err, connect.CodeInternal, "can't update the flag")
	})
}

// mustGet gets the flag with id and returns the response's flag, failing the
// test if the call fails or the response carries no flag.
func mustGet(t *testing.T, client flagconnect.FlagServiceClient, id string) *flag.Flag {
	t.Helper()
	res, err := client.Get(context.Background(), &flag.GetRequest{Id: id})
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
			_, err := client.Get(ctx, &flag.GetRequest{Id: tc.id})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
		})
	}

	t.Run("rejects an id no flag has", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "new-checkout")
		_, err := client.Get(ctx, &flag.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeNotFound, "can't get the flag")
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, unreachableDB)
		_, err := unreachable.Get(ctx, &flag.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeInternal, "can't get the flag")
	})
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
			_, err := svc.Update(ctx, &flag.UpdateRequest{Id: id, Key: "new-checkout"})
			return err
		},
		"Get": func(id string) error {
			_, err := svc.Get(ctx, &flag.GetRequest{Id: id})
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
