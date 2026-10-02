package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
)

// Backup writes a consistent copy of the database to
// dir/chokominto-<name>.db using VACUUM INTO, which works while the server
// keeps running. An existing file with the same name is replaced.
func (db *DB) Backup(ctx context.Context, dir, name string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, "chokominto-"+name+".db")
	tmp := final + ".tmp"
	os.Remove(tmp)
	if _, err := db.backupConn().ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := syncFile(tmp); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, nil
}

func syncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// PruneBackups keeps the newest keep daily backups (chokominto-YYYY-MM-DD.db)
// in dir and removes older ones. Backups taken before migrations are kept.
func PruneBackups(dir string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dir, "chokominto-????-??-??.db"))
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	for _, m := range matches[min(keep, len(matches)):] {
		if err := os.Remove(m); err != nil {
			return err
		}
	}
	return nil
}

// backupConn is a connection of its own once the database is open, so a
// backup (a long read in WAL mode) never holds up incoming listens. During
// migrations, before it exists, the writer is used.
func (db *DB) backupConn() *sql.DB {
	if db.bk != nil {
		return db.bk
	}
	return db.w
}
