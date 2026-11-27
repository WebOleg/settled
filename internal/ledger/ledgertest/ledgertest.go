// Package ledgertest gives each test a database of its own on the throwaway PostgreSQL
// that make check-go starts, migrated, with a login role that holds only the role the
// code under test runs as.
package ledgertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// AdminEnv names the connection string of a PostgreSQL superuser for the tests.
// Without it the tests fail rather than skip: a ledger left untested reads as a ledger
// that passed.
const AdminEnv = "SETTLED_TEST_POSTGRES"

// DB is one test's database.
type DB struct {
	// OwnerURL connects as the owner of the schema.
	OwnerURL string
	// AppURL connects as a login role that holds only the role given to New.
	AppURL string
}

// New creates a database, migrates it with migrate, and makes a login role with
// grant. Both go when the test ends.
func New(t *testing.T, migrate func(context.Context, string) (int, error), grant string) DB {
	t.Helper()
	adminURL := os.Getenv(AdminEnv)
	if adminURL == "" {
		t.Fatalf("%s is not set; run make check-go", AdminEnv)
	}
	ctx := context.Background()
	server, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("ledgertest: connect as admin: %v", err)
	}
	defer server.Close(ctx)
	suffix := randomHex(t, 6)
	db, role := "test_"+suffix, "test_app_"+suffix
	password := randomHex(t, 16)
	for _, q := range []string{
		"CREATE DATABASE " + db,
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s'", role, password),
	} {
		if _, err := server.Exec(ctx, q); err != nil {
			t.Fatalf("ledgertest: create the database and its role: %v", err)
		}
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)")
		_, _ = c.Exec(context.Background(), "DROP ROLE IF EXISTS "+role)
	})
	owner := withDatabase(t, adminURL, db, "", "")
	if _, err := migrate(ctx, owner); err != nil {
		t.Fatalf("ledgertest: migrate: %v", err)
	}
	conn, err := pgx.Connect(ctx, owner)
	if err != nil {
		t.Fatalf("ledgertest: connect to the test database: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf("GRANT %s TO %s", grant, role)); err != nil {
		t.Fatalf("ledgertest: grant %s: %v", grant, err)
	}
	return DB{OwnerURL: owner, AppURL: withDatabase(t, adminURL, db, role, password)}
}

func withDatabase(t *testing.T, raw, db, user, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("ledgertest: parse %s: %v", AdminEnv, err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}
