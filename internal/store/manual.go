package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"chokominto/internal/names"
)

// SearchRecordings finds recordings whose song, credited artists or album
// have a name containing q, in any script: the query is matched against
// match keys, romaji typed in Latin letters against the romanized kana
// names and guessed kanji readings, and kana typed against Latin names.
// Recordings no received text is linked to anymore are left out. Most
// listened first.
func (db *DB) SearchRecordings(ctx context.Context, userID int64, q string, limit int) ([]RecordingCount, error) {
	key := names.MatchKey(q)
	if strings.TrimSpace(q) == "" || key == "" {
		return nil, nil
	}
	like := "%" + escapeLike(key) + "%"
	romaji := "%" + escapeLike(names.FoldLongVowels(key)) + "%"
	if rk := names.RomajiKey(q); rk != "" {
		romaji = "%" + escapeLike(rk) + "%"
	}
	const hit = `(match_key LIKE ?1 ESCAPE '\' OR match_key LIKE ?2 ESCAPE '\' OR romaji_key LIKE ?2 ESCAPE '\' OR guess_key LIKE ?2 ESCAPE '\')`
	return db.recordingCounts(ctx, `SELECT r.id,
		coalesce((SELECT sum(n) FROM listen_totals WHERE user_id = ?3 AND recording_id = r.id), 0) AS n
		FROM recordings r WHERE r.user_id = ?3 AND r.merged_into IS NULL
		-- A recording nothing is linked to is left over from a relink.
		AND EXISTS (SELECT 1 FROM sources WHERE recording_id = r.id) AND (
		  r.song_id IN (SELECT song_id FROM song_aliases WHERE `+hit+`)
		  OR r.id IN (SELECT rc.recording_id FROM recording_credits rc JOIN artist_aliases ON artist_aliases.artist_id = rc.artist_id
		              WHERE rc.role IN ('main', 'featured') AND `+hit+`)
		  OR r.id IN (SELECT rt.recording_id FROM release_tracks rt JOIN release_aliases ON release_aliases.release_id = rt.release_id
		              WHERE `+hit+`))
		ORDER BY n DESC, r.id LIMIT ?4`, like, romaji, userID, limit)
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ErrNameClash means the text a manual scrobble would use already belongs
// to a different recording.
var ErrNameClash = errors.New("another recording already has this exact name")

// ScrobbleRecording adds a listen of a recording by hand. The listen's text
// is the recording's names (and its album when it's on exactly one), and it
// is linked to the recording in the same transaction, so background linking
// never has to guess.
func (db *DB) ScrobbleRecording(ctx context.Context, userID, recordingID, at int64) (RecordingInfo, error) {
	infos, err := db.RecordingInfos(ctx, []int64{recordingID})
	if err != nil {
		return RecordingInfo{}, err
	}
	info, ok := infos[recordingID]
	if !ok {
		return info, ErrNotFound
	}
	var owner int64
	if err := db.r.QueryRowContext(ctx, `SELECT user_id FROM recordings WHERE id = ?`, recordingID).Scan(&owner); err != nil || owner != userID {
		return info, ErrNotFound
	}
	var releaseID int64
	var album string
	rels, err := db.refs(ctx, `SELECT rl.id, rl.name, rl.other_names FROM release_tracks rt JOIN releases rl ON rl.id = rt.release_id
		WHERE rt.recording_id = ? AND rl.merged_into IS NULL`, recordingID)
	if err != nil {
		return info, err
	}
	if len(rels) == 1 {
		releaseID, album = rels[0].ID, rels[0].Name
	}
	artist := JoinNames(info.Artists)
	title := info.Song.Name
	if info.Version != "" {
		title += " (" + info.Version + ")"
	}
	payload, _ := json.Marshal(map[string]any{
		"manual":       true,
		"recording_id": recordingID,
		"track_metadata": map[string]any{
			"artist_name": artist, "track_name": title, "release_name": album,
		},
	})

	err = db.Write(ctx, func(tx *sql.Tx) error {
		src, err := upsertSource(ctx, tx, userID, SourceText{Artist: artist, Title: title, Album: album})
		if err != nil {
			return err
		}
		if src.recordingID.Valid && src.recordingID.Int64 != recordingID {
			return ErrNameClash
		}
		if !src.recordingID.Valid {
			s := Source{ID: src.id, UserID: userID, Artist: artist, Title: title, Album: album}
			if err := LinkSourceTx(ctx, tx, s, recordingID, releaseID); err != nil {
				return err
			}
			src.recordingID = sql.NullInt64{Int64: recordingID, Valid: true}
			if releaseID != 0 {
				src.releaseID = sql.NullInt64{Int64: releaseID, Valid: true}
			}
		}
		var listenID int64
		err = tx.QueryRowContext(ctx,
			`INSERT INTO listens (user_id, listened_at, source_id, origin, received_at, recording_id, release_id)
			 VALUES (?, ?, ?, 'manual', ?, ?, ?) ON CONFLICT DO NOTHING RETURNING id`,
			userID, at, src.id, unix(), src.recordingID, src.releaseID).Scan(&listenID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already scrobbled at that exact time
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO listen_payloads (listen_id, payload) VALUES (?, ?)`, listenID, string(payload))
		return err
	})
	return info, err
}
