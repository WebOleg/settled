// Command ledger-migrate brings the ledger database to the latest schema
// (internal/ledger's migrations) as the owner of the schema, and makes the login roles
// the services connect as, each holding only the role it needs. Passwords are read
// from files and never printed. Run again, it changes nothing that is in place, and
// sets each login's password from its file.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/WebOleg/settled/internal/ledger"
)

// logins collects -login flags: name:role:password-file.
type logins []login

type login struct{ name, role, file string }

func (l *logins) String() string { return fmt.Sprint(*l) }

func (l *logins) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) != 3 || !identifier.MatchString(parts[0]) || !identifier.MatchString(parts[1]) || parts[2] == "" {
		return fmt.Errorf("-login %q: want name:role:password-file", v)
	}
	*l = append(*l, login{parts[0], parts[1], parts[2]})
	return nil
}

var identifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func main() {
	db := flag.String("db", "postgres://admin@127.0.0.1:15432/ledger?sslmode=disable", "the ledger database, as its owner, without the password")
	passwordFile := flag.String("db-password-file", ".devnet/bridge/postgres-admin.txt", "file holding the owner's password")
	var ls logins
	flag.Var(&ls, "login", "a login role to make, name:role:password-file; repeatable")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, *db, *passwordFile, ls); err != nil {
		fmt.Fprintf(os.Stderr, "ledger-migrate: FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, db, passwordFile string, ls logins) error {
	dsn, err := ledger.DSN(db, passwordFile)
	if err != nil {
		return err
	}
	applied, err := ledger.Migrate(ctx, dsn)
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var version int
	if err := conn.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		return fmt.Errorf("read the schema version: %w", err)
	}
	fmt.Printf("schema           version %d, %d applied now\n", version, applied)
	for _, l := range ls {
		pw, err := ledger.ReadPassword(l.file)
		if err != nil {
			return fmt.Errorf("login %s: %w", l.name, err)
		}
		// Names come from flags checked against identifier and the password is hex
		// (ledger.ReadPassword), so neither can carry SQL. DDL takes no parameters, so
		// they are written into the text.
		stmts := []string{
			fmt.Sprintf(`DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE %s LOGIN; END IF; END $$`, l.name, l.name),
			fmt.Sprintf("ALTER ROLE %s LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD '%s'", l.name, pw),
			fmt.Sprintf("GRANT %s TO %s", l.role, l.name),
		}
		for _, q := range stmts {
			if _, err := conn.Exec(ctx, q); err != nil {
				return fmt.Errorf("login %s: %w", l.name, err)
			}
		}
		fmt.Printf("login            %s, holds %s\n", l.name, l.role)
	}
	fmt.Println("ledger-migrate: ok")
	return nil
}
