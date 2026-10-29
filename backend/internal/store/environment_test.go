package store

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	"github.com/nazifbara/flagsvc/backend/internal/database"
)

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

// newTestStore connects to TEST_DB_URL and starts from empty tables.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbURL := os.Getenv("TEST_DB_URL")
	if dbURL == "" {
		t.Fatal("TEST_DB_URL not set")
	}
	store, err := New(dbURL)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	t.Cleanup(func() { store.db.Close() })
	resetDB(t, store.db)
	return store
}

// wantConnectErr fails the test unless err is a *connect.Error with want's
// code and message.
func wantConnectErr(t *testing.T, err error, want *connect.Error) {
	t.Helper()
	if err == nil {
		t.Fatalf("error = nil, want error = %q, code = %d", want.Error(), want.Code())
	}
	got, ok := err.(*connect.Error)
	if !ok {
		t.Fatalf("error = %v (%T), want *connect.Error", err, err)
	}
	if got.Code() != want.Code() || got.Error() != want.Error() {
		t.Fatalf("error, code = %v, %d, want error, code = %v, %d", got.Error(), got.Code(), want.Error(), want.Code())
	}
}

func TestEnvironmentCreation(t *testing.T) {
	store := newTestStore(t)

	empty := connect.NewError(connect.CodeInvalidArgument, "can't create an environment with an empty name")
	tooLong := connect.NewError(connect.CodeInvalidArgument, "can't create an environment with a name over 100 characters")
	tests := map[string]struct {
		name string
		// want is the stored name, when it differs from name.
		want string
		err  *connect.Error
	}{
		"reject empty name": {
			name: "",
			err:  empty,
		},
		// A name with only whitespace is as empty as "".
		"reject a single space": {
			name: " ",
			err:  empty,
		},
		"reject only spaces": {
			name: "    ",
			err:  empty,
		},
		"reject tabs and newlines": {
			name: "\t\n\r ",
			err:  empty,
		},
		"reject only non-breaking spaces": {
			name: "  ",
			err:  empty,
		},
		"accept a one character name": {
			name: "a",
		},
		"accept a name with inner spaces": {
			name: "staging eu",
		},
		// The column is VARCHAR(100): 100 characters fit, 101 must be
		// rejected before they reach the database.
		"accept a 100 character name": {
			name: strings.Repeat("a", 100),
		},
		"reject name over 100 characters": {
			name: strings.Repeat("a", 101),
			err:  tooLong,
		},
		// The limit counts characters, not bytes: "é" is 2 bytes.
		"accept 100 multibyte characters": {
			name: strings.Repeat("é", 100),
		},
		"reject 101 multibyte characters": {
			name: strings.Repeat("é", 101),
			err:  tooLong,
		},
		// Surrounding whitespace is trimmed before the limit is checked.
		"accept 100 characters with surrounding whitespace": {
			name: "  " + strings.Repeat("a", 100) + "  ",
			want: strings.Repeat("a", 100),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			got, err := store.CreateEnv(context.Background(), test.name)
			if test.err != nil {
				wantConnectErr(t, err, test.err)
				return
			}
			if err != nil {
				t.Fatalf("CreateEnv() unexpected error: %v", err)
			}
			wantName := test.want
			if wantName == "" {
				wantName = test.name
			}
			// The ID is random, so it is only checked for being set.
			want := database.Environment{Name: wantName}
			if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(database.Environment{}, "ID")); diff != "" {
				t.Fatalf("CreateEnv() mismatch (-want +got):\n%s", diff)
			}
			if got.ID == (database.Environment{}).ID {
				t.Fatal("CreateEnv() returned a zero ID")
			}
		})
	}
}

// Names are stored trimmed and lowercased, both in what CreateEnv returns
// and in the row it writes.
func TestEnvironmentCreationNormalizesName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tests := map[string]struct{ name, want string }{
		"capitalized":          {"Prod", "prod"},
		"all caps":             {"PRODUCTION", "production"},
		"mixed case":           {"sTaGiNg", "staging"},
		"non-ASCII letters":    {"ÉCOLE", "école"},
		"dotted capital I":     {"İzmir", "izmir"},
		"surrounding spaces":   {"  prod  ", "prod"},
		"tabs and newlines":    {"\tprod\n", "prod"},
		"spaces and case":      {" Staging EU ", "staging eu"},
		"inner spaces kept":    {"staging  eu", "staging  eu"},
		"already normalized":   {"prod", "prod"},
		"100 uppercase letter": {strings.Repeat("A", 100), strings.Repeat("a", 100)},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			got, err := store.CreateEnv(ctx, test.name)
			if err != nil {
				t.Fatalf("CreateEnv(%q): %v", test.name, err)
			}
			if got.Name != test.want {
				t.Errorf("CreateEnv(%q).Name = %q, want %q", test.name, got.Name, test.want)
			}
			var stored string
			if err := store.db.QueryRowContext(ctx, "SELECT name FROM environments WHERE id = $1", got.ID).Scan(&stored); err != nil {
				t.Fatalf("reading back environment %v: %v", got.ID, err)
			}
			if stored != test.want {
				t.Errorf("stored name = %q, want %q", stored, test.want)
			}
		})
	}
}

// Names are unique regardless of case: "Prod" and "prod" are the same
// environment.
func TestEnvironmentCreationRejectsDuplicateName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	exists := connect.NewError(connect.CodeAlreadyExists, connect.CodeAlreadyExists.String())

	tests := map[string]struct{ first, second string }{
		"same name":           {"production", "production"},
		"capitalized":         {"prod", "Prod"},
		"lowercased":          {"Prod", "prod"},
		"all caps":            {"production", "PRODUCTION"},
		"mixed case":          {"staging", "sTaGiNg"},
		"non-ASCII letters":   {"école", "ÉCOLE"},
		"inner spaces differ": {"staging eu", "Staging EU"},
		"surrounding spaces":  {"prod", "  prod  "},
		"tabs and newlines":   {"prod", "\tprod\n"},
		"spaces and case":     {"prod", " PROD "},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resetDB(t, store.db)
			if _, err := store.CreateEnv(ctx, test.first); err != nil {
				t.Fatalf("CreateEnv(%q): %v", test.first, err)
			}
			_, err := store.CreateEnv(ctx, test.second)
			wantConnectErr(t, err, exists)
		})
	}

	// A different name is still accepted, with its own ID.
	t.Run("different name accepted", func(t *testing.T) {
		resetDB(t, store.db)
		first, err := store.CreateEnv(ctx, "production")
		if err != nil {
			t.Fatalf("CreateEnv() first call: %v", err)
		}
		other, err := store.CreateEnv(ctx, "staging")
		if err != nil {
			t.Fatalf("CreateEnv() different name: %v", err)
		}
		if other.ID == first.ID {
			t.Fatalf("CreateEnv() reused ID %v", first.ID)
		}
	})
}

func TestGetEnv(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Several environments, so a lookup that ignores the ID and returns
	// any row is caught.
	created := make(map[string]database.Environment)
	for _, name := range []string{"development", "staging", "production"} {
		env, err := store.CreateEnv(ctx, name)
		if err != nil {
			t.Fatalf("CreateEnv(%q): %v", name, err)
		}
		created[name] = env
	}

	for name, want := range created {
		t.Run("finds "+name+" by its ID", func(t *testing.T) {
			got, err := store.GetEnv(ctx, want.ID)
			if err != nil {
				t.Fatalf("GetEnv(%v): %v", want.ID, err)
			}
			if diff := cmp.Diff(want, got); diff != "" {
				t.Fatalf("GetEnv(%v) mismatch (-want +got):\n%s", want.ID, diff)
			}
		})
	}

	// The name comes back as it was stored, trimmed and lowercased.
	t.Run("returns the normalized name", func(t *testing.T) {
		env, err := store.CreateEnv(ctx, "  QA  ")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		got, err := store.GetEnv(ctx, env.ID)
		if err != nil {
			t.Fatalf("GetEnv(%v): %v", env.ID, err)
		}
		if got.Name != "qa" {
			t.Fatalf("GetEnv(%v).Name = %q, want %q", env.ID, got.Name, "qa")
		}
	})

	notFound := connect.NewError(connect.CodeNotFound, connect.CodeNotFound.String())
	t.Run("unknown ID is not found", func(t *testing.T) {
		got, err := store.GetEnv(ctx, uuid.New())
		wantConnectErr(t, err, notFound)
		if got != (database.Environment{}) {
			t.Fatalf("GetEnv() = %+v on error, want the zero Environment", got)
		}
	})
	t.Run("zero ID is not found", func(t *testing.T) {
		_, err := store.GetEnv(ctx, uuid.Nil)
		wantConnectErr(t, err, notFound)
	})
	t.Run("deleted environment is not found", func(t *testing.T) {
		env, err := store.CreateEnv(ctx, "temporary")
		if err != nil {
			t.Fatalf("CreateEnv(): %v", err)
		}
		resetDB(t, store.db)
		_, err = store.GetEnv(ctx, env.ID)
		wantConnectErr(t, err, notFound)
	})
}

// Failures that are neither a missing row nor a Postgres error still
// surface as errors with a matching code, never as an empty environment.
func TestGetEnvReportsNonDBErrors(t *testing.T) {
	store := newTestStore(t)
	env, err := store.CreateEnv(context.Background(), "production")
	if err != nil {
		t.Fatalf("CreateEnv(): %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := map[string]struct {
		ctx  context.Context
		want *connect.Error
	}{
		"canceled context": {
			ctx:  canceled,
			want: connect.NewError(connect.CodeCanceled, connect.CodeCanceled.String()),
		},
		"expired deadline": {
			ctx:  expired,
			want: connect.NewError(connect.CodeDeadlineExceeded, connect.CodeDeadlineExceeded.String()),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := store.GetEnv(test.ctx, env.ID)
			wantConnectErr(t, err, test.want)
			if got != (database.Environment{}) {
				t.Fatalf("GetEnv() = %+v on error, want the zero Environment", got)
			}
		})
	}

	// Run last: it closes the connection pool the other cases use.
	t.Run("closed database is internal", func(t *testing.T) {
		if err := store.db.Close(); err != nil {
			t.Fatalf("closing the database: %v", err)
		}
		got, err := store.GetEnv(context.Background(), env.ID)
		wantConnectErr(t, err, connect.NewError(connect.CodeInternal, connect.CodeInternal.String()))
		if got != (database.Environment{}) {
			t.Fatalf("GetEnv() = %+v on error, want the zero Environment", got)
		}
	})
}
