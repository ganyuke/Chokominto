package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func suggestions(t *testing.T, db *DB) []string {
	t.Helper()
	rows, err := db.r.Query(`SELECT kind, a_id, b_id, reason, status FROM suggestions ORDER BY kind, a_id, b_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var kind, reason, status string
		var a, b int64
		rows.Scan(&kind, &a, &b, &reason, &status)
		out = append(out, fmt.Sprintf("%s %d-%d %s %s", kind, a, b, reason, status))
	}
	return out
}

func TestFindSuggestions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	var aidoru, aidoruSingle, tvSize int64
	db.Write(ctx, func(tx *sql.Tx) error {
		aidoru, _ = CreateRecordingTx(ctx, tx, m.u, "Aidoru", []Credit{{m.yoasobiFW, "main"}})
		tvSize, _ = CreateRecordingTx(ctx, tx, m.u, "アイドル (TV size)", []Credit{{m.yoasobiFW, "main"}})
		aidoruSingle, _ = CreateReleaseTx(ctx, tx, m.u, "Aidoru", []int64{m.yoasobiFW})
		return nil
	})
	if err := db.FindSuggestions(ctx, m.u); err != nil {
		t.Fatal(err)
	}
	got := suggestions(t, db)
	for _, want := range []string{
		fmt.Sprintf("artist %d-%d %s open", m.yoasobiFW, m.yoasobi, ReasonKey),
		fmt.Sprintf("recording %d-%d %s open", m.idolJP, m.demo, ReasonCover),
		fmt.Sprintf("recording %d-%d %s open", m.idolJP, aidoru, ReasonRomaji),
		fmt.Sprintf("recording %d-%d %s open", m.idolJP, tvSize, ReasonBrackets),
		fmt.Sprintf("release %d-%d %s open", m.single, aidoruSingle, ReasonRomaji),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in\n%v", want, got)
		}
	}
	// Idol and アイドル can't be told apart from text alone.
	for _, s := range got {
		if strings.HasPrefix(s, fmt.Sprintf("recording %d-%d ", m.idolJP, m.idolEN)) {
			t.Error("suggested Idol for アイドル")
		}
	}

	// A dismissed pair stays dismissed when suggestions are looked for again.
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE suggestions SET status = 'dismissed' WHERE kind = 'artist'`)
		return err
	})
	if err := db.FindSuggestions(ctx, m.u); err != nil {
		t.Fatal(err)
	}
	if got := suggestions(t, db); !slices.Contains(got, fmt.Sprintf("artist %d-%d %s dismissed", m.yoasobiFW, m.yoasobi, ReasonKey)) {
		t.Errorf("dismissed pair came back: %v", got)
	}
}

func suggestionID(t *testing.T, db *DB, kind string, a, b int64) int64 {
	t.Helper()
	var id int64
	if err := db.r.QueryRow(`SELECT id FROM suggestions WHERE kind = ? AND a_id = ? AND b_id = ?`, kind, min(a, b), max(a, b)).Scan(&id); err != nil {
		t.Fatalf("no suggestion %s %d-%d", kind, a, b)
	}
	return id
}

func TestAnswerSuggestions(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	if err := db.FindSuggestions(ctx, m.u); err != nil {
		t.Fatal(err)
	}
	start := snapshot(t, db)
	cover := suggestionID(t, db, "recording", m.idolJP, m.demo)
	artist := suggestionID(t, db, "artist", m.yoasobiFW, m.yoasobi)

	// "Same song, different version" only fits songs.
	if _, _, err := db.AnswerSuggestions(ctx, m.u, []int64{cover, artist}, AnswerVersion); !errors.Is(err, ErrAnswer) {
		t.Fatalf("version for an artist: %v", err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatal("refused answer changed data")
	}

	// The demo becomes a version of アイドル: one song, two recordings.
	edit, n, err := db.AnswerSuggestions(ctx, m.u, []int64{cover}, AnswerVersion)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if m.song(t, db, m.demo) != m.song(t, db, m.idolJP) {
		t.Fatal("not one song")
	}
	if _, err := db.UndoEdit(ctx, m.u, edit); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatalf("after undo\n%s\nwant\n%s", got, start)
	}

	// Two answers at once are one edit.
	edit, n, err = db.AnswerSuggestions(ctx, m.u, []int64{cover, artist}, AnswerDifferent)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if got := count(t, db, `SELECT count(*) FROM suggestions WHERE status = 'dismissed'`); got != 2 {
		t.Fatalf("%d dismissed", got)
	}
	if _, total, _ := db.OpenSuggestions(ctx, m.u, 100); total != 0 {
		t.Fatalf("%d still open", total)
	}
	if _, err := db.UndoEdit(ctx, m.u, edit); err != nil {
		t.Fatal(err)
	}
	if got := snapshot(t, db); got != start {
		t.Fatal("undo didn't reopen them")
	}

	// Merging keeps the side with more listens: THE BOOK 3's YOASOBI.
	if _, _, err := db.AnswerSuggestions(ctx, m.u, []int64{artist}, AnswerSame); err != nil {
		t.Fatal(err)
	}
	var merged int64
	db.r.QueryRow(`SELECT merged_into FROM artists WHERE id = ?`, m.yoasobiFW).Scan(&merged)
	if merged != m.yoasobi {
		t.Fatalf("merged into %d", merged)
	}
	if _, total, _ := db.OpenSuggestions(ctx, m.u, 100); total != 1 {
		t.Fatalf("%d open after the merge, want the cover", total)
	}
}

// Soundtracks split by composer before are suggested as one album.
func TestSuggestSplitSoundtrack(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	m := newMusic(t, db)
	var a, b, best1, best2 int64
	db.Write(ctx, func(tx *sql.Tx) error {
		a, _ = CreateReleaseTx(ctx, tx, m.u, "Atelier Ryza Original Soundtrack", []int64{m.ayase})
		b, _ = CreateReleaseTx(ctx, tx, m.u, "Atelier Ryza Original Soundtrack", []int64{m.kg})
		best1, _ = CreateReleaseTx(ctx, tx, m.u, "Best", []int64{m.ayase})
		best2, _ = CreateReleaseTx(ctx, tx, m.u, "Best", []int64{m.kg})
		return nil
	})
	if err := db.FindSuggestions(ctx, m.u); err != nil {
		t.Fatal(err)
	}
	got := suggestions(t, db)
	if !slices.Contains(got, fmt.Sprintf("release %d-%d %s open", a, b, ReasonAlbum)) {
		t.Errorf("split soundtrack not suggested: %v", got)
	}
	for _, s := range got {
		if strings.HasPrefix(s, fmt.Sprintf("release %d-%d ", best1, best2)) {
			t.Errorf("suggested two bands' Best: %s", s)
		}
	}
}
