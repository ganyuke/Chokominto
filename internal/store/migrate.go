package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: name must start with a number and _", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		b, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{v, e.Name(), string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: expected version %d", m.name, i+1)
		}
	}
	return ms, nil
}

// SchemaVersion is the version the code expects.
func SchemaVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		panic(err)
	}
	return len(ms)
}

// NeedsUpdate reports whether opening the existing database file will
// migrate it, so callers can say so before a long wait. A missing file
// needs no update, it's created fresh.
func NeedsUpdate(ctx context.Context, file string) (bool, error) {
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	c, err := sql.Open("sqlite", dsn(file, true))
	if err != nil {
		return false, err
	}
	defer c.Close()
	var current int
	if err := c.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return false, err
	}
	return current > 0 && current < SchemaVersion(), nil
}

func (db *DB) migrate(ctx context.Context, backupDir string) error {
	ms, err := loadMigrations()
	if err != nil {
		return err
	}
	var current int
	if err := db.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return err
	}
	if current > len(ms) {
		return fmt.Errorf("database is at schema version %d but this build only knows %d. Use a newer build or restore a backup", current, len(ms))
	}
	if current == len(ms) {
		return nil
	}
	// There are no down migrations. The way back is this backup.
	if current > 0 && backupDir != "" {
		if _, err := db.Backup(ctx, backupDir, fmt.Sprintf("before-v%d", current+1)); err != nil {
			return fmt.Errorf("backup before migration: %w", err)
		}
	}
	for _, m := range ms[current:] {
		if err := db.runMigration(ctx, m); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// rebuildMarker in a migration turns foreign key enforcement off while it
// runs, which SQLite needs for rebuilding a table that others refer to
// (https://sqlite.org/lang_altertable.html#otheralter). Every reference is
// checked before the migration commits.
const rebuildMarker = "-- migrate: rebuilds tables"

func (db *DB) runMigration(ctx context.Context, m migration) error {
	rebuild := strings.Contains(m.sql, rebuildMarker)
	if rebuild {
		// The pragma is a no-op inside a transaction, so it's set on the
		// single write connection first. If that connection is replaced in
		// between, enforcement is back on and the rebuild fails safely.
		if _, err := db.w.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			return err
		}
		defer db.w.ExecContext(context.WithoutCancel(ctx), "PRAGMA foreign_keys = ON")
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		// Another process may have opened the same new or old database at
		// the same moment, like the service starting while the first
		// account is created. Write transactions take turns, so the version
		// read here is settled, and a migration already done is skipped.
		var current int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
			return err
		}
		if current >= m.version {
			return nil
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return err
		}
		if rebuild {
			var table string
			var rowid, parent, fkid any
			err := tx.QueryRowContext(ctx, "PRAGMA foreign_key_check").Scan(&table, &rowid, &parent, &fkid)
			if err == nil {
				return fmt.Errorf("broken reference from %s row %v to %v", table, rowid, parent)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version))
		return err
	})
}
