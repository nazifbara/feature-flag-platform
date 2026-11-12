package envservice

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
	"uuid"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/validate"
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/store"
	envv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/env/v1"
	"github.com/nazifbara/feature-flag-platform/gen/go/ffp/env/v1/envv1connect"
	"google.golang.org/protobuf/proto"

	_ "github.com/lib/pq"
)

// testDBLock is the session advisory lock every package testing against
// TEST_DB_URL holds for its whole run. go test runs packages in parallel, and
// these tests empty the tables and count rows, so they'd break each other.
// The store and flagservice tests hold the same one.
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

// newStore opens a store at dbURL.
func newStore(t *testing.T, dbURL string) *store.Store {
	t.Helper()
	st, err := store.New(dbURL)
	if err != nil {
		t.Fatalf("store.New(): %v", err)
	}
	return st
}

// newClient serves a Service backed by st through a real connect server, with
// the validation interceptor the server runs, and returns a client for it.
func newClient(t *testing.T, st *store.Store) envv1connect.EnvServiceClient {
	t.Helper()
	server := connect.NewServer(validate.NewServerInterceptor())
	envv1connect.RegisterEnvServiceHandler(server, New(st))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return envv1connect.NewEnvServiceClient(
		connect.NewClient(connecthttp.NewTransport(ts.Client(), ts.URL)),
	)
}

// unreachableDB is a database URL nothing listens on, for a store whose every
// query fails.
const unreachableDB = "postgres://nobody@127.0.0.1:1/none?sslmode=disable&connect_timeout=2"

// count gives the number of rows in table.
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
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

// mustCreate creates an environment named name and returns the response's
// environment, failing the test if the call fails or the response carries
// none.
func mustCreate(t *testing.T, client envv1connect.EnvServiceClient, name string) *envv1.Environment {
	t.Helper()
	res, err := client.Create(context.Background(), &envv1.CreateRequest{Name: name})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	if res.GetEnvironment() == nil {
		t.Fatalf("Create(%q) returned no environment", name)
	}
	return res.GetEnvironment()
}

// settingRow is a flag's setting in an environment, as stored.
type settingRow struct {
	flagID           string
	enabled          bool
	fallthroughValue float64
}

// settingsIn reads the settings stored for the environment with envID,
// ordered by flag id.
func settingsIn(t *testing.T, db *sql.DB, envID string) []settingRow {
	t.Helper()
	rows, err := db.Query("SELECT flag_id, enabled, fallthrough FROM flag_settings WHERE environment_id = $1 ORDER BY flag_id", envID)
	if err != nil {
		t.Fatalf("reading the settings of %s: %v", envID, err)
	}
	defer rows.Close()
	var settings []settingRow
	for rows.Next() {
		var s settingRow
		if err := rows.Scan(&s.flagID, &s.enabled, &s.fallthroughValue); err != nil {
			t.Fatalf("reading a setting of %s: %v", envID, err)
		}
		settings = append(settings, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the settings of %s: %v", envID, err)
	}
	return settings
}

func TestCreate(t *testing.T) {
	db := testDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	t.Run("returns the stored environment", func(t *testing.T) {
		resetDB(t, db)
		before := dbNow(t, db)
		got := mustCreate(t, client, "production")
		after := dbNow(t, db)

		id, err := uuid.Parse(got.Id)
		if err != nil {
			t.Fatalf("id %q isn't a UUID: %v", got.Id, err)
		}
		if got.Name != "production" {
			t.Errorf("name = %q, want %q", got.Name, "production")
		}
		if got.Version != 1 {
			t.Errorf("version = %d, want 1", got.Version)
		}
		created, updated := got.GetCreatedAt().AsTime(), got.GetUpdatedAt().AsTime()
		if created.Before(before) || created.After(after) {
			t.Errorf("created_at = %v, want between %v and %v", created, before, after)
		}
		if !updated.Equal(created) {
			t.Errorf("updated_at = %v, want created_at %v", updated, created)
		}

		var name string
		var version int64
		var storedCreated, storedUpdated time.Time
		err = db.QueryRow("SELECT name, version, created_at, updated_at FROM environments WHERE id = $1", id).
			Scan(&name, &version, &storedCreated, &storedUpdated)
		if err != nil {
			t.Fatalf("reading the environment back: %v", err)
		}
		if name != got.Name || version != got.Version || !storedCreated.Equal(created) || !storedUpdated.Equal(updated) {
			t.Errorf("stored environment = %q, %d, %v, %v, want the response's %q, %d, %v, %v",
				name, version, storedCreated, storedUpdated, got.Name, got.Version, created, updated)
		}
	})

	t.Run("gives each environment its own id", func(t *testing.T) {
		resetDB(t, db)
		first := mustCreate(t, client, "staging")
		second := mustCreate(t, client, "production")
		if first.Id == second.Id {
			t.Fatalf("both environments got id %q", first.Id)
		}
	})

	// The response carries the name as stored, not as sent.
	t.Run("returns the normalized name", func(t *testing.T) {
		resetDB(t, db)
		got := mustCreate(t, client, "  Production  ")
		if got.Name != "production" {
			t.Fatalf("name = %q, want %q", got.Name, "production")
		}
	})

	// Every existing flag starts out disabled, serving nothing, in the new
	// environment.
	t.Run("gives every flag a disabled setting", func(t *testing.T) {
		resetDB(t, db)
		var want []settingRow
		for _, key := range []string{"new-checkout", "dark-mode"} {
			f, err := st.CreateFlag(ctx, key)
			if err != nil {
				t.Fatalf("CreateFlag(%q): %v", key, err)
			}
			want = append(want, settingRow{flagID: f.ID.String()})
		}
		sort.Slice(want, func(i, j int) bool { return want[i].flagID < want[j].flagID })
		env := mustCreate(t, client, "production")
		got := settingsIn(t, db, env.Id)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("settings = %+v, want %+v", got, want)
		}
	})

	// Flags created before it bump only the environments that existed then:
	// a new environment starts at version 1 whatever happened before it.
	t.Run("starts at version 1 after other changes", func(t *testing.T) {
		resetDB(t, db)
		staging := mustCreate(t, client, "staging")
		for _, key := range []string{"new-checkout", "dark-mode"} {
			if _, err := st.CreateFlag(ctx, key); err != nil {
				t.Fatalf("CreateFlag(%q): %v", key, err)
			}
		}
		var stagingVersion int64
		if err := db.QueryRow("SELECT version FROM environments WHERE id = $1", staging.Id).Scan(&stagingVersion); err != nil {
			t.Fatalf("reading staging's version: %v", err)
		}
		if stagingVersion == 1 {
			t.Fatal("staging is still at version 1 after creating flags; the case needs another environment's version to have moved")
		}
		if got := mustCreate(t, client, "production"); got.Version != 1 {
			t.Fatalf("version = %d, want 1", got.Version)
		}
	})

	t.Run("gives no settings without flags", func(t *testing.T) {
		resetDB(t, db)
		env := mustCreate(t, client, "production")
		if got := settingsIn(t, db, env.Id); len(got) != 0 {
			t.Fatalf("settings = %+v, want none", got)
		}
	})

	// Rejected names write nothing, whether the proto rules or the store
	// catch them.
	rejected := map[string]struct {
		name    string
		code    connect.Code
		message string
	}{
		// The proto's min_len.
		"an empty name": {
			name: "",
			code: connect.CodeInvalidArgument,
		},
		// The proto has no max_len: the store checks the length once the name
		// is trimmed.
		"a name over 100 characters": {
			name:    strings.Repeat("a", 101),
			code:    connect.CodeInvalidArgument,
			message: "can't create an environment with a name over 100 characters",
		},
		// The column counts characters, so this is over 100 of them however
		// few bytes they'd take.
		"a name over 100 characters of two bytes each": {
			name:    strings.Repeat("é", 101),
			code:    connect.CodeInvalidArgument,
			message: "can't create an environment with a name over 100 characters",
		},
		// Passes min_len, so the store's check catches it.
		"a name of only spaces": {
			name:    "   ",
			code:    connect.CodeInvalidArgument,
			message: "can't create an environment with an empty name",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			resetDB(t, db)
			if _, err := st.CreateFlag(ctx, "new-checkout"); err != nil {
				t.Fatalf("CreateFlag(): %v", err)
			}
			_, err := client.Create(ctx, &envv1.CreateRequest{Name: tc.name})
			wantCode(t, err, tc.code, tc.message)
			if n := count(t, db, "environments"); n != 0 {
				t.Errorf("%d environments after a rejected create, want 0", n)
			}
			if n := count(t, db, "flag_settings"); n != 0 {
				t.Errorf("%d settings after a rejected create, want 0", n)
			}
		})
	}

	// The limit counts characters, not bytes, once the name is trimmed.
	for name, tc := range map[string]struct{ name, want string }{
		"accepts a name of exactly 100 characters": {
			name: strings.Repeat("a", 100),
			want: strings.Repeat("a", 100),
		},
		"accepts a name of 100 characters over 100 bytes": {
			name: strings.Repeat("é", 100),
			want: strings.Repeat("é", 100),
		},
		"accepts a name of 100 characters once trimmed": {
			name: "  " + strings.Repeat("a", 100) + "  ",
			want: strings.Repeat("a", 100),
		},
	} {
		t.Run(name, func(t *testing.T) {
			resetDB(t, db)
			if got := mustCreate(t, client, tc.name); got.Name != tc.want {
				t.Fatalf("name = %q, want %q", got.Name, tc.want)
			}
		})
	}

	// Names are compared after normalizing, so case doesn't make a new
	// environment.
	t.Run("rejects a name already taken", func(t *testing.T) {
		resetDB(t, db)
		if _, err := st.CreateFlag(ctx, "new-checkout"); err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		mustCreate(t, client, "production")
		_, err := client.Create(ctx, &envv1.CreateRequest{Name: "Production"})
		wantCode(t, err, connect.CodeAlreadyExists, "can't create the environment")
		if n := count(t, db, "environments"); n != 1 {
			t.Errorf("%d environments after the duplicate, want 1", n)
		}
		if n := count(t, db, "flag_settings"); n != 1 {
			t.Errorf("%d settings after the duplicate, want 1", n)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, unreachableDB))
		_, err := unreachable.Create(ctx, &envv1.CreateRequest{Name: "production"})
		wantCode(t, err, connect.CodeInternal, "can't create the environment")
	})
}

// badIDs are environment ids the proto's uuid rule rejects, with the message
// the client gets. The rule takes only the dashed form, unlike uuid.Parse.
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

// mustGet gets the environment with id and returns the response's
// environment, failing the test if the call fails or the response carries
// none.
func mustGet(t *testing.T, client envv1connect.EnvServiceClient, id string) *envv1.Environment {
	t.Helper()
	res, err := client.Get(context.Background(), &envv1.GetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if res.GetEnvironment() == nil {
		t.Fatalf("Get(%s) returned no environment", id)
	}
	return res.GetEnvironment()
}

func TestGet(t *testing.T) {
	db := testDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	t.Run("returns the stored environment", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "production")
		if got := mustGet(t, client, created.Id); !proto.Equal(got, created) {
			t.Fatalf("Get() = %v, want the created environment %v", got, created)
		}
	})

	// Every flag created bumps every environment, so Get shows the version
	// SDKs sync to, past the 1 that Create returned.
	t.Run("returns the current version", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "production")
		for _, key := range []string{"new-checkout", "dark-mode"} {
			if _, err := st.CreateFlag(ctx, key); err != nil {
				t.Fatalf("CreateFlag(%q): %v", key, err)
			}
		}
		var version int64
		if err := db.QueryRow("SELECT version FROM environments WHERE id = $1", created.Id).Scan(&version); err != nil {
			t.Fatalf("reading the version: %v", err)
		}
		if version == created.Version {
			t.Fatalf("version still %d after creating flags; the case needs it to have moved", version)
		}
		got := mustGet(t, client, created.Id)
		if got.Version != version {
			t.Errorf("version = %d, want the stored %d", got.Version, version)
		}
		if got.Id != created.Id || got.Name != created.Name || !proto.Equal(got.CreatedAt, created.CreatedAt) {
			t.Errorf("Get() = %v, want %v but for its version and updated_at", got, created)
		}
	})

	t.Run("returns the environment asked for among others", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "staging")
		production := mustCreate(t, client, "production")
		mustCreate(t, client, "development")
		if got := mustGet(t, client, production.Id); !proto.Equal(got, production) {
			t.Fatalf("Get() = %v, want %v", got, production)
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		resetDB(t, db)
		created := mustCreate(t, client, "production")
		if got := mustGet(t, client, strings.ToUpper(created.Id)); got.Id != created.Id {
			t.Fatalf("id = %s, want %s", got.Id, created.Id)
		}
	})

	for name, tc := range badIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.Get(ctx, &envv1.GetRequest{Id: tc.id})
			wantCode(t, err, connect.CodeInvalidArgument, tc.message)
		})
	}

	t.Run("rejects an id no environment has", func(t *testing.T) {
		resetDB(t, db)
		mustCreate(t, client, "production")
		_, err := client.Get(ctx, &envv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeNotFound, "can't get the environment")
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, unreachableDB))
		_, err := unreachable.Get(ctx, &envv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		wantCode(t, err, connect.CodeInternal, "can't get the environment")
	})
}

// Without the validation interceptor, as for a direct caller, each handler's
// own parsing still rejects an id that isn't a UUID, before reaching the
// store: the store here can't reach a database, and would answer Internal.
func TestHandlersWithoutValidationRejectBadIDs(t *testing.T) {
	svc := New(newStore(t, unreachableDB))
	ctx := context.Background()
	calls := map[string]func(id string) error{
		"Get": func(id string) error {
			_, err := svc.Get(ctx, &envv1.GetRequest{Id: id})
			return err
		},
	}
	for rpc, call := range calls {
		for _, id := range []string{"", "not-a-uuid", " 6ba7b810-9dad-11d1-80b4-00c04fd430c8 "} {
			t.Run(fmt.Sprintf("%s %q", rpc, id), func(t *testing.T) {
				wantCode(t, call(id), connect.CodeInvalidArgument, "invalid environment id")
			})
		}
	}
}
