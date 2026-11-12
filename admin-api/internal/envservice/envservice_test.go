package envservice

import (
	"context"
	"database/sql"
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
	"github.com/nazifbara/feature-flag-platform/admin-api/internal/testutil"
	environmentv1 "github.com/nazifbara/feature-flag-platform/gen/go/ffp/environment/v1"
	"github.com/nazifbara/feature-flag-platform/gen/go/ffp/environment/v1/environmentv1connect"
	"google.golang.org/protobuf/proto"
)

// TestMain runs the tests while holding testutil.DBLock.
func TestMain(m *testing.M) { testutil.Main(m) }

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
func newClient(t *testing.T, st *store.Store) environmentv1connect.EnvironmentServiceClient {
	t.Helper()
	server := connect.NewServer(validate.NewServerInterceptor())
	environmentv1connect.RegisterEnvironmentServiceHandler(server, New(st))
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return environmentv1connect.NewEnvironmentServiceClient(
		connect.NewClient(connecthttp.NewTransport(ts.Client(), ts.URL)),
	)
}

// count gives the number of rows in table.
func count(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// mustCreate creates an environment named name and returns the response's
// environment, failing the test if the call fails or the response carries
// none.
func mustCreate(t *testing.T, client environmentv1connect.EnvironmentServiceClient, name string) *environmentv1.Environment {
	t.Helper()
	res, err := client.Create(context.Background(), &environmentv1.CreateRequest{Name: name})
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
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	t.Run("returns the stored environment", func(t *testing.T) {
		testutil.ResetDB(t, db)
		before := testutil.DBNow(t, db)
		got := mustCreate(t, client, "production")
		after := testutil.DBNow(t, db)

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
		testutil.ResetDB(t, db)
		first := mustCreate(t, client, "staging")
		second := mustCreate(t, client, "production")
		if first.Id == second.Id {
			t.Fatalf("both environments got id %q", first.Id)
		}
	})

	// The response carries the name as stored, not as sent.
	t.Run("returns the normalized name", func(t *testing.T) {
		testutil.ResetDB(t, db)
		got := mustCreate(t, client, "  Production  ")
		if got.Name != "production" {
			t.Fatalf("name = %q, want %q", got.Name, "production")
		}
	})

	// Every existing flag starts out disabled, serving nothing, in the new
	// environment.
	t.Run("gives every flag a disabled setting", func(t *testing.T) {
		testutil.ResetDB(t, db)
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
		testutil.ResetDB(t, db)
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
		testutil.ResetDB(t, db)
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
			testutil.ResetDB(t, db)
			if _, err := st.CreateFlag(ctx, "new-checkout"); err != nil {
				t.Fatalf("CreateFlag(): %v", err)
			}
			_, err := client.Create(ctx, &environmentv1.CreateRequest{Name: tc.name})
			testutil.WantCode(t, err, tc.code, tc.message)
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
			testutil.ResetDB(t, db)
			if got := mustCreate(t, client, tc.name); got.Name != tc.want {
				t.Fatalf("name = %q, want %q", got.Name, tc.want)
			}
		})
	}

	// Names are compared after normalizing, so case doesn't make a new
	// environment.
	t.Run("rejects a name already taken", func(t *testing.T) {
		testutil.ResetDB(t, db)
		if _, err := st.CreateFlag(ctx, "new-checkout"); err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		mustCreate(t, client, "production")
		_, err := client.Create(ctx, &environmentv1.CreateRequest{Name: "Production"})
		testutil.WantCode(t, err, connect.CodeAlreadyExists, "can't create the environment")
		if n := count(t, db, "environments"); n != 1 {
			t.Errorf("%d environments after the duplicate, want 1", n)
		}
		if n := count(t, db, "flag_settings"); n != 1 {
			t.Errorf("%d settings after the duplicate, want 1", n)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.Create(ctx, &environmentv1.CreateRequest{Name: "production"})
		testutil.WantCode(t, err, connect.CodeInternal, "can't create the environment")
	})
}

// mustGet gets the environment with id and returns the response's
// environment, failing the test if the call fails or the response carries
// none.
func mustGet(t *testing.T, client environmentv1connect.EnvironmentServiceClient, id string) *environmentv1.Environment {
	t.Helper()
	res, err := client.Get(context.Background(), &environmentv1.GetRequest{Id: id})
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if res.GetEnvironment() == nil {
		t.Fatalf("Get(%s) returned no environment", id)
	}
	return res.GetEnvironment()
}

func TestGet(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	t.Run("returns the stored environment", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "production")
		if got := mustGet(t, client, created.Id); !proto.Equal(got, created) {
			t.Fatalf("Get() = %v, want the created environment %v", got, created)
		}
	})

	// Every flag created bumps every environment, so Get shows the version
	// SDKs sync to, past the 1 that Create returned.
	t.Run("returns the current version", func(t *testing.T) {
		testutil.ResetDB(t, db)
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
		testutil.ResetDB(t, db)
		mustCreate(t, client, "staging")
		production := mustCreate(t, client, "production")
		mustCreate(t, client, "development")
		if got := mustGet(t, client, production.Id); !proto.Equal(got, production) {
			t.Fatalf("Get() = %v, want %v", got, production)
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "production")
		if got := mustGet(t, client, strings.ToUpper(created.Id)); got.Id != created.Id {
			t.Fatalf("id = %s, want %s", got.Id, created.Id)
		}
	})

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.Get(ctx, &environmentv1.GetRequest{Id: tc.ID})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, tc.Message)
		})
	}

	t.Run("rejects an id no environment has", func(t *testing.T) {
		testutil.ResetDB(t, db)
		mustCreate(t, client, "production")
		_, err := client.Get(ctx, &environmentv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		testutil.WantCode(t, err, connect.CodeNotFound, "can't get the environment")
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.Get(ctx, &environmentv1.GetRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8"})
		testutil.WantCode(t, err, connect.CodeInternal, "can't get the environment")
	})
}

// envRow is an environment as stored.
type envRow struct {
	name             string
	version          int64
	created, updated time.Time
}

// readEnv reads the environment with id from db.
func readEnv(t *testing.T, db *sql.DB, id string) envRow {
	t.Helper()
	var r envRow
	err := db.QueryRow("SELECT name, version, created_at, updated_at FROM environments WHERE id = $1", id).
		Scan(&r.name, &r.version, &r.created, &r.updated)
	if err != nil {
		t.Fatalf("reading environment %s: %v", id, err)
	}
	return r
}

// mustUpdate gives the environment with id the name and returns the
// response's environment, failing the test if the call fails or the response
// carries none.
func mustUpdate(t *testing.T, client environmentv1connect.EnvironmentServiceClient, id, name string) *environmentv1.Environment {
	t.Helper()
	res, err := client.Update(context.Background(), &environmentv1.UpdateRequest{Id: id, Name: name})
	if err != nil {
		t.Fatalf("Update(%s, %q): %v", id, name, err)
	}
	if res.GetEnvironment() == nil {
		t.Fatalf("Update(%s, %q) returned no environment", id, name)
	}
	return res.GetEnvironment()
}

func TestUpdate(t *testing.T) {
	db := testutil.OpenDB(t)
	st := newStore(t, os.Getenv("TEST_DB_URL"))
	client := newClient(t, st)
	ctx := context.Background()

	// Only the name and updated_at move.
	t.Run("returns the renamed environment", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "staging")
		before := testutil.DBNow(t, db)
		got := mustUpdate(t, client, created.Id, "production")
		after := testutil.DBNow(t, db)

		if got.Id != created.Id {
			t.Errorf("id = %s, want %s", got.Id, created.Id)
		}
		if got.Name != "production" {
			t.Errorf("name = %q, want %q", got.Name, "production")
		}
		if got.Version != created.Version {
			t.Errorf("version = %d, want %d unchanged", got.Version, created.Version)
		}
		if !proto.Equal(got.CreatedAt, created.CreatedAt) {
			t.Errorf("created_at = %v, want %v unchanged", got.CreatedAt.AsTime(), created.CreatedAt.AsTime())
		}
		if updated := got.GetUpdatedAt().AsTime(); updated.Before(before) || updated.After(after) {
			t.Errorf("updated_at = %v, want between %v and %v", updated, before, after)
		}

		stored := readEnv(t, db, created.Id)
		want := envRow{name: got.Name, version: got.Version, created: got.CreatedAt.AsTime(), updated: got.UpdatedAt.AsTime()}
		if !stored.created.Equal(want.created) || !stored.updated.Equal(want.updated) || stored.name != want.name || stored.version != want.version {
			t.Errorf("stored environment = %+v, want the response's %+v", stored, want)
		}
		if g := mustGet(t, client, created.Id); !proto.Equal(g, got) {
			t.Errorf("Get() = %v, want the updated environment %v", g, got)
		}
	})

	// The response carries the name as stored, not as sent.
	t.Run("returns the normalized name", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "staging")
		if got := mustUpdate(t, client, created.Id, "  Production  "); got.Name != "production" {
			t.Fatalf("name = %q, want %q", got.Name, "production")
		}
	})

	t.Run("renames only the environment asked for", func(t *testing.T) {
		testutil.ResetDB(t, db)
		first := mustCreate(t, client, "staging")
		second := mustCreate(t, client, "production")
		third := mustCreate(t, client, "development")
		mustUpdate(t, client, second.Id, "prod")
		for _, e := range []*environmentv1.Environment{first, third} {
			if got := mustGet(t, client, e.Id); !proto.Equal(got, e) {
				t.Errorf("Get(%s) = %v, want %v unchanged", e.Name, got, e)
			}
		}
	})

	// An environment's own name isn't taken from it, whatever its case.
	t.Run("accepts the name it already has", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "production")
		if got := mustUpdate(t, client, created.Id, "Production"); got.Name != "production" {
			t.Fatalf("name = %q, want %q", got.Name, "production")
		}
	})

	// SDKs sync by the environment's id, and nothing they evaluate holds its
	// name, so a rename has nothing for them to pick up.
	t.Run("leaves the version, settings and change logs alone", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "staging")
		if _, err := st.CreateFlag(ctx, "new-checkout"); err != nil {
			t.Fatalf("CreateFlag(): %v", err)
		}
		before := readEnv(t, db, created.Id)
		settingsBefore := settingsIn(t, db, created.Id)
		logsBefore := count(t, db, "change_logs")
		mustUpdate(t, client, created.Id, "production")
		if got := readEnv(t, db, created.Id).version; got != before.version {
			t.Errorf("version = %d, want %d unchanged", got, before.version)
		}
		if got := settingsIn(t, db, created.Id); fmt.Sprint(got) != fmt.Sprint(settingsBefore) {
			t.Errorf("settings = %+v, want %+v unchanged", got, settingsBefore)
		}
		if got := count(t, db, "change_logs"); got != logsBefore {
			t.Errorf("%d change log entries, want %d", got, logsBefore)
		}
	})

	t.Run("accepts the id in upper case", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "staging")
		if got := mustUpdate(t, client, strings.ToUpper(created.Id), "production"); got.Id != created.Id {
			t.Fatalf("id = %s, want %s", got.Id, created.Id)
		}
	})

	// The limit counts characters once the name is trimmed, as in Create.
	t.Run("accepts a name of 100 characters once trimmed", func(t *testing.T) {
		testutil.ResetDB(t, db)
		created := mustCreate(t, client, "staging")
		name := strings.Repeat("é", 100)
		if got := mustUpdate(t, client, created.Id, "  "+name+"  "); got.Name != name {
			t.Fatalf("name = %q, want %q", got.Name, name)
		}
	})

	// Rejected updates leave the environment as it was.
	rejected := map[string]struct {
		id      string
		name    string
		code    connect.Code
		message string
	}{
		"an id no environment has": {
			id:      "6ba7b810-9dad-11d1-80b4-00c04fd430c8",
			name:    "production",
			code:    connect.CodeNotFound,
			message: "can't update the environment",
		},
		// The proto's min_len.
		"an empty name": {
			name: "",
			code: connect.CodeInvalidArgument,
		},
		// Passes min_len, so the store's checks catch these.
		"a name of only spaces": {
			name:    "   ",
			code:    connect.CodeInvalidArgument,
			message: "can't update an environment with an empty name",
		},
		"a name over 100 characters": {
			name:    strings.Repeat("a", 101),
			code:    connect.CodeInvalidArgument,
			message: "can't update an environment with a name over 100 characters",
		},
	}
	for name, tc := range rejected {
		t.Run("rejects "+name, func(t *testing.T) {
			testutil.ResetDB(t, db)
			created := mustCreate(t, client, "staging")
			before := readEnv(t, db, created.Id)
			id := created.Id
			if tc.id != "" {
				id = tc.id
			}
			_, err := client.Update(ctx, &environmentv1.UpdateRequest{Id: id, Name: tc.name})
			testutil.WantCode(t, err, tc.code, tc.message)
			if after := readEnv(t, db, created.Id); after != before {
				t.Fatalf("environment = %+v after a rejected update, want %+v unchanged", after, before)
			}
		})
	}

	for name, tc := range testutil.BadIDs {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := client.Update(ctx, &environmentv1.UpdateRequest{Id: tc.ID, Name: "production"})
			testutil.WantCode(t, err, connect.CodeInvalidArgument, tc.Message)
		})
	}

	// Names are compared after normalizing, so case doesn't dodge a taken
	// name.
	t.Run("rejects a name another environment has", func(t *testing.T) {
		testutil.ResetDB(t, db)
		mustCreate(t, client, "production")
		other := mustCreate(t, client, "staging")
		before := readEnv(t, db, other.Id)
		_, err := client.Update(ctx, &environmentv1.UpdateRequest{Id: other.Id, Name: "Production"})
		testutil.WantCode(t, err, connect.CodeAlreadyExists, "can't update the environment")
		if after := readEnv(t, db, other.Id); after != before {
			t.Fatalf("environment = %+v after a rejected update, want %+v unchanged", after, before)
		}
	})

	// The client gets the store's message, never the database's error.
	t.Run("hides database failures", func(t *testing.T) {
		unreachable := newClient(t, newStore(t, testutil.UnreachableDB))
		_, err := unreachable.Update(ctx, &environmentv1.UpdateRequest{Id: "6ba7b810-9dad-11d1-80b4-00c04fd430c8", Name: "production"})
		testutil.WantCode(t, err, connect.CodeInternal, "can't update the environment")
	})
}

// Without the validation interceptor, as for a direct caller, each handler's
// own parsing still rejects an id that isn't a UUID, before reaching the
// store: the store here can't reach a database, and would answer Internal.
func TestHandlersWithoutValidationRejectBadIDs(t *testing.T) {
	svc := New(newStore(t, testutil.UnreachableDB))
	ctx := context.Background()
	calls := map[string]func(id string) error{
		"Get": func(id string) error {
			_, err := svc.Get(ctx, &environmentv1.GetRequest{Id: id})
			return err
		},
		"Update": func(id string) error {
			_, err := svc.Update(ctx, &environmentv1.UpdateRequest{Id: id, Name: "production"})
			return err
		},
	}
	for rpc, call := range calls {
		for _, id := range testutil.UnparsableIDs {
			t.Run(fmt.Sprintf("%s %q", rpc, id), func(t *testing.T) {
				testutil.WantCode(t, call(id), connect.CodeInvalidArgument, "invalid environment id")
			})
		}
	}
}
