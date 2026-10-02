// Package store owns the SQLite database: opening it, migrations, backups,
// and every query.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"database/sql/driver"

	"modernc.org/sqlite"

	"chokominto/internal/names"
)

// match_key(text) in SQL is names.MatchKey, for searching received text,
// which has no stored keys.
func init() {
	sqlite.MustRegisterDeterministicScalarFunction("match_key", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		s, _ := args[0].(string)
		return names.MatchKey(s), nil
	})
}

// DB wraps pools on the same file. SQLite allows one writer at a time, so
// all writes go through a single connection and never wait on each other
// for locks. Reads use a small separate pool and see a consistent WAL
// snapshot. Backups get a connection of their own.
type DB struct {
	Path string
	w    *sql.DB
	r    *sql.DB
	bk   *sql.DB // backups only, see backupConn
}

// ErrNotFound is returned by lookups that match no row.
var ErrNotFound = errors.New("not found")

func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	// FULL rather than NORMAL: a Pi on an SD card can lose power, and the
	// write rate of a scrobble server is tiny.
	q.Add("_pragma", "synchronous(FULL)")
	if readOnly {
		q.Add("_pragma", "query_only(ON)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

// pingNew connects the writer. Switching a new database to WAL fails at
// once, without waiting, if another process is switching it at the same
// moment, so that is retried for as long as a busy database would be.
func pingNew(ctx context.Context, w *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := w.PingContext(ctx)
		if err == nil || !strings.Contains(err.Error(), "database is locked") || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Open opens (creating if needed) the database at path and runs pending
// migrations. backupDir receives a copy of an existing database before any
// migration is applied.
func Open(ctx context.Context, path, backupDir string) (*DB, error) {
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxIdleTime(0)
	if err := pingNew(ctx, w); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db := &DB{Path: path, w: w}
	if err := db.migrate(ctx, backupDir); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	db.r = r
	bk, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	bk.SetMaxOpenConns(1)
	db.bk = bk
	return db, nil
}

func (db *DB) Close() error {
	return errors.Join(db.bk.Close(), db.r.Close(), db.w.Close())
}

// Write runs fn in a write transaction. Transactions start IMMEDIATE, so a
// read inside fn is never upgraded into a lock conflict.
func (db *DB) Write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// A panic in fn must not leave the transaction open: there's one
	// writer, so every later write, scrobbles included, would wait on it
	// for good.
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Reader returns the read-only pool.
func (db *DB) Reader() *sql.DB { return db.r }

// now is replaced in tests.
var now = time.Now

func unix() int64 { return now().Unix() }
