package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
)

// NewListen is one listen as received, ready to store.
type NewListen struct {
	ListenedAt int64
	Artist     string
	Title      string
	Album      string
	// AlbumArtist is the album's artist when the scrobbler sent one
	// separately (Web Scrobbler's release_artist_name), else "".
	AlbumArtist string
	Payload     []byte // the listen object exactly as received
	Incomplete  bool
}

type InsertResult struct {
	Stored     int
	Duplicates int
}

// InsertListens stores listens in one transaction. A listen with the same
// user, time and exact text as an existing one (including a deleted one) is
// a duplicate and is skipped, which makes client retries and repeated
// imports harmless.
func (db *DB) InsertListens(ctx context.Context, userID int64, origin string, tokenID *int64, ls []NewListen) (InsertResult, error) {
	var res InsertResult
	received := unix()
	err := db.Write(ctx, func(tx *sql.Tx) error {
		res = InsertResult{}
		sources := map[SourceText]sourceRow{}
		for _, l := range ls {
			key := SourceText{l.Artist, l.Title, l.Album, l.AlbumArtist}
			src, ok := sources[key]
			if !ok {
				var err error
				src, err = upsertSource(ctx, tx, userID, key)
				if err != nil {
					return err
				}
				sources[key] = src
			}
			var listenID int64
			err := tx.QueryRowContext(ctx,
				`INSERT INTO listens (user_id, listened_at, source_id, origin, token_id, received_at, incomplete, recording_id, release_id)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
				 ON CONFLICT (user_id, listened_at, source_id) DO NOTHING
				 RETURNING id`,
				userID, l.ListenedAt, src.id, origin, tokenID, received, l.Incomplete, src.recordingID, src.releaseID).Scan(&listenID)
			if errors.Is(err, sql.ErrNoRows) {
				res.Duplicates++
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO listen_payloads (listen_id, payload) VALUES (?, ?)`, listenID, string(l.Payload)); err != nil {
				return err
			}
			res.Stored++
		}
		return nil
	})
	return res, err
}

// SourceText is received text exactly as sent. Each distinct SourceText is
// one source.
type SourceText struct {
	Artist, Title, Album, AlbumArtist string
}

const sourceTextMatch = `user_id = ? AND artist_text = ? AND title_text = ? AND album_text = ? AND album_artist_text = ?`

func upsertSource(ctx context.Context, tx *sql.Tx, userID int64, t SourceText) (sourceRow, error) {
	var s sourceRow
	err := tx.QueryRowContext(ctx,
		`SELECT id, msid, recording_id, release_id FROM sources WHERE `+sourceTextMatch,
		userID, t.Artist, t.Title, t.Album, t.AlbumArtist).Scan(&s.id, &s.msid, &s.recordingID, &s.releaseID)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return s, err
	}
	s.msid = newUUID()
	err = tx.QueryRowContext(ctx,
		`INSERT INTO sources (user_id, artist_text, title_text, album_text, album_artist_text, msid, search_key) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		userID, t.Artist, t.Title, t.Album, t.AlbumArtist, s.msid, SearchKey(t)).Scan(&s.id)
	if err != nil {
		return s, err
	}
	// New text: link it to a song in the background.
	return s, EnqueueTx(ctx, tx, "resolve", fmt.Sprintf("source:%d", s.id), "", 0)
}

type sourceRow struct {
	id          int64
	msid        string
	recordingID sql.NullInt64
	releaseID   sql.NullInt64
}

// SourceMSID returns the msid of existing source text, if it was ever received.
func (db *DB) SourceMSID(ctx context.Context, userID int64, t SourceText) (string, error) {
	var msid string
	err := db.r.QueryRowContext(ctx,
		`SELECT msid FROM sources WHERE `+sourceTextMatch,
		userID, t.Artist, t.Title, t.Album, t.AlbumArtist).Scan(&msid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return msid, err
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Listen is a stored listen with its text as received.
type Listen struct {
	ID          int64
	ListenedAt  int64
	ReceivedAt  int64
	Artist      string
	Title       string
	Album       string
	MSID        string
	Origin      string
	Incomplete  bool
	RecordingID int64 // 0 while not linked
	ReleaseID   int64 // 0 when there's no album
}

const listenCols = `l.id, l.listened_at, l.received_at, s.artist_text, s.title_text, s.album_text, s.msid, l.origin, l.incomplete,
	coalesce(l.recording_id, 0), coalesce(l.release_id, 0)`

func (db *DB) scanListens(ctx context.Context, q string, args ...any) ([]Listen, error) {
	rows, err := db.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ls []Listen
	for rows.Next() {
		var l Listen
		if err := rows.Scan(&l.ID, &l.ListenedAt, &l.ReceivedAt, &l.Artist, &l.Title, &l.Album, &l.MSID, &l.Origin, &l.Incomplete, &l.RecordingID, &l.ReleaseID); err != nil {
			return nil, err
		}
		ls = append(ls, l)
	}
	return ls, rows.Err()
}

// Cursor is a position in a user's listens, ordered by (time, id).
type Cursor struct {
	TS int64
	ID int64
}

// CursorAt is a cursor that sits between all listens at ts and before ts,
// for time-only bounds: Before: CursorAt(t) means "earlier than t".
func CursorAt(ts int64) Cursor { return Cursor{ts, 0} }

// CursorAfter is a cursor after every listen at ts: After: CursorAfter(t)
// means "later than t".
func CursorAfter(ts int64) Cursor { return Cursor{ts, math.MaxInt64} }

// ListenRange selects listens strictly between After and Before. Results are
// always newest first. By default the newest Limit listens in the range are
// returned. With Oldest, the oldest Limit are returned instead, which is how
// paging toward newer listens works.
type ListenRange struct {
	Before *Cursor
	After  *Cursor
	Oldest bool
	Limit  int
}

func (db *DB) Listens(ctx context.Context, userID int64, r ListenRange) ([]Listen, error) {
	var where []string
	args := []any{userID}
	if r.Before != nil {
		where = append(where, "(l.listened_at, l.id) < (?, ?)")
		args = append(args, r.Before.TS, r.Before.ID)
	}
	if r.After != nil {
		where = append(where, "(l.listened_at, l.id) > (?, ?)")
		args = append(args, r.After.TS, r.After.ID)
	}
	order := "DESC"
	if r.Oldest {
		order = "ASC"
	}
	q := `SELECT ` + listenCols + `
	      FROM listens l JOIN sources s ON s.id = l.source_id
	      WHERE l.user_id = ? AND l.deleted_by IS NULL`
	for _, w := range where {
		q += " AND " + w
	}
	q += fmt.Sprintf(" ORDER BY l.listened_at %s, l.id %s LIMIT ?", order, order)
	args = append(args, r.Limit)

	ls, err := db.scanListens(ctx, q, args...)
	if r.Oldest {
		for i, j := 0, len(ls)-1; i < j; i, j = i+1, j-1 {
			ls[i], ls[j] = ls[j], ls[i]
		}
	}
	return ls, err
}

// ListenStats returns the number of listens and the oldest and newest
// listen times (0 when there are none).
//
// The count comes from user_stats, kept exact by triggers. min() and max()
// are separate queries on purpose: SQLite answers a lone min() or max() with
// one index lookup, but not when they're combined.
func (db *DB) ListenStats(ctx context.Context, userID int64) (count int, oldest, newest int64, err error) {
	const live = ` FROM listens WHERE user_id = ? AND deleted_by IS NULL`
	var o, n sql.NullInt64
	err = db.r.QueryRowContext(ctx, `SELECT listen_count FROM user_stats WHERE user_id = ?`, userID).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return
	}
	if err = db.r.QueryRowContext(ctx, `SELECT min(listened_at)`+live, userID).Scan(&o); err != nil {
		return
	}
	err = db.r.QueryRowContext(ctx, `SELECT max(listened_at)`+live, userID).Scan(&n)
	return count, o.Int64, n.Int64, err
}

// FindListen finds a live listen by its time and source msid.
func (db *DB) FindListen(ctx context.Context, userID, listenedAt int64, msid string) (Listen, error) {
	ls, err := db.scanListens(ctx, `SELECT `+listenCols+` FROM listens l JOIN sources s ON s.id = l.source_id
		 WHERE l.user_id = ? AND l.listened_at = ? AND s.msid = ? AND l.deleted_by IS NULL LIMIT 1`, userID, listenedAt, msid)
	if err != nil {
		return Listen{}, err
	}
	if len(ls) == 0 {
		return Listen{}, ErrNotFound
	}
	return ls[0], nil
}

// DeleteListen hides a listen through the edit log, so it can be undone.
func (db *DB) DeleteListen(ctx context.Context, userID int64, l Listen) (int64, error) {
	summary := fmt.Sprintf("Deleted listen: %s by %s", orDash(l.Title), orDash(l.Artist))
	return db.ApplyEdit(ctx, userID, "delete-listen", summary, func(editID int64) []Change {
		return []Change{{
			Table:  "listens",
			ID:     l.ID,
			Before: map[string]any{"deleted_by": nil},
			After:  map[string]any{"deleted_by": editID},
		}}
	})
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}
