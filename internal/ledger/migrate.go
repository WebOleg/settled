package ledger

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationLock is the advisory lock key held while migrating, so two processes never
// apply the same migration at once: the first 8 bytes of sha256("settled ledger").
const migrationLock int64 = 0x2a5c1f0d6e8b9a37

// ErrMigrationChanged is returned when a migration already applied no longer matches
// the file in the repository: the schema in the database is not the one the code
// describes, and nothing is applied on top of it.
var ErrMigrationChanged = errors.New("ledger: an applied migration was changed")

type migration struct {
	version int
	name    string
	sql     string
	sum     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, fmt.Errorf("ledger: read migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("ledger: migration %s is not named NNNN_name.sql", name)
		}
		raw, err := migrations.ReadFile(path.Join("migrations", name))
		if err != nil {
			return nil, fmt.Errorf("ledger: read migration %s: %w", name, err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, migration{version: v, name: name, sql: string(raw), sum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("ledger: migration %s: versions must run 1, 2, 3 without gaps", m.name)
		}
	}
	return out, nil
}

// Migrate brings the schema to the latest migration, each in its own transaction, under
// an advisory lock. It is run by the owner of the schema, not by the service, which only
// holds ledger_writer. A migration once applied is checked against its file by sha256.
func Migrate(ctx context.Context, url string) (applied int, err error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return 0, fmt.Errorf("ledger: connect to migrate: %w", err)
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return 0, fmt.Errorf("ledger: take the migration lock: %w", err)
	}
	// Closing the connection releases the lock too; unlocking first only frees it sooner.
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version integer PRIMARY KEY,
		name text NOT NULL,
		sha256 text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return 0, fmt.Errorf("ledger: create schema_migrations: %w", err)
	}
	done := map[int]string{}
	rows, err := conn.Query(ctx, "SELECT version, sha256 FROM schema_migrations")
	if err != nil {
		return 0, fmt.Errorf("ledger: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return 0, fmt.Errorf("ledger: read schema_migrations: %w", err)
		}
		done[v] = sum
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("ledger: read schema_migrations: %w", err)
	}
	for v := range done {
		if v > len(ms) {
			return 0, fmt.Errorf("ledger: the database has migration %d, the code only %d", v, len(ms))
		}
	}
	for _, m := range ms {
		if sum, ok := done[m.version]; ok {
			if sum != m.sum {
				return applied, fmt.Errorf("%w: %s", ErrMigrationChanged, m.name)
			}
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, name, sha256) VALUES ($1, $2, $3)", m.version, m.name, m.sum)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("ledger: apply %s: %w", m.name, err)
		}
		applied++
	}
	return applied, nil
}
