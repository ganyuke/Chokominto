package resolve

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"chokominto/internal/store"
)

// TestHistoryCases runs real received text through the resolver and checks
// what it links to. The cases come from the owner's own listening history,
// so they live outside the repository: set CHOKOMINTO_HISTORY_CASES to the
// file to run them.
//
// The file is tab-separated with a header row and these columns:
//
//	artist, title, album, album_artist  text as received
//	song                                expected song name, "-" for unlinked
//	main, featured                      expected artists, joined with " | "
//	release                             expected album name, "" for none
//	key                                 rows with the same key must be one song
//
// Rows are scrobbled in file order, so earlier rows can create what later
// rows link to.
//
// With CHOKOMINTO_HISTORY_RECORD set to a file name, the test writes the
// same rows with what the resolver does now, for drafting expectations to
// check by hand.
func TestHistoryCases(t *testing.T) {
	path := os.Getenv("CHOKOMINTO_HISTORY_CASES")
	if path == "" {
		t.Skip("CHOKOMINTO_HISTORY_CASES not set")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = '\t'
	r.LazyQuotes = true
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[h] = i
	}
	for _, h := range []string{"artist", "title", "album", "album_artist", "song", "main", "featured", "release", "key"} {
		if _, ok := col[h]; !ok {
			t.Fatalf("column %s missing", h)
		}
	}
	var rows []map[string]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		row := map[string]string{}
		for h, i := range col {
			if i < len(rec) {
				row[h] = rec[i]
			}
		}
		rows = append(rows, row)
	}

	e := newEnv(t)
	ctx := context.Background()
	for _, row := range rows {
		e.ts += 60
		if _, err := e.db.InsertListens(ctx, e.user, "listenbrainz", nil, []store.NewListen{{
			ListenedAt: e.ts, Artist: row["artist"], Title: row["title"], Album: row["album"],
			AlbumArtist: row["album_artist"], Payload: []byte(`{}`),
		}}); err != nil {
			t.Fatal(err)
		}
	}
	e.run.Drain(ctx)

	var record *csv.Writer
	if out := os.Getenv("CHOKOMINTO_HISTORY_RECORD"); out != "" {
		f, err := os.Create(out)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		record = csv.NewWriter(f)
		record.Comma = '\t'
		defer record.Flush()
		record.Write(header)
	}

	songOf := map[string]string{} // key -> song id
	failed := 0
	for i, row := range rows {
		var songID int64
		var song, release, main, featured string
		err := e.db.Reader().QueryRow(`SELECT coalesce(r.song_id, 0), coalesce(sg.name, '-'), coalesce(rl.name, ''),
			coalesce((SELECT group_concat(name, ' | ') FROM (SELECT a.name FROM recording_credits rc JOIN artists a ON a.id = rc.artist_id
				WHERE rc.recording_id = s.recording_id AND rc.role = 'main' ORDER BY rc.position)), ''),
			coalesce((SELECT group_concat(name, ' | ') FROM (SELECT a.name FROM recording_credits rc JOIN artists a ON a.id = rc.artist_id
				WHERE rc.recording_id = s.recording_id AND rc.role = 'featured' ORDER BY rc.position)), '')
			FROM sources s
			LEFT JOIN recordings r ON r.id = s.recording_id LEFT JOIN songs sg ON sg.id = r.song_id
			LEFT JOIN releases rl ON rl.id = s.release_id
			WHERE s.user_id = ? AND s.artist_text = ? AND s.title_text = ? AND s.album_text = ? AND s.album_artist_text = ?`,
			e.user, row["artist"], row["title"], row["album"], row["album_artist"]).Scan(&songID, &song, &release, &main, &featured)
		if err != nil {
			t.Fatalf("row %d: %v", i+2, err)
		}
		if record != nil {
			got := map[string]string{"song": song, "main": main, "featured": featured, "release": release, "key": row["key"]}
			rec := make([]string, len(header))
			for i, h := range header {
				if v, ok := got[h]; ok {
					rec[i] = v
				} else {
					rec[i] = row[h]
				}
			}
			record.Write(rec)
			continue
		}
		var diffs []string
		check := func(what, got, want string) {
			if got != want {
				diffs = append(diffs, fmt.Sprintf("%s %q, want %q", what, got, want))
			}
		}
		check("song", song, row["song"])
		if song != "-" {
			check("main", main, row["main"])
			check("featured", featured, row["featured"])
			check("release", release, row["release"])
		}
		if k := row["key"]; k != "" && songID != 0 {
			id := strconv.FormatInt(songID, 10)
			if prev, ok := songOf[k]; ok && prev != id {
				diffs = append(diffs, "not the same song as the other rows with key "+k)
			}
			songOf[k] = id
		}
		if len(diffs) > 0 {
			failed++
			t.Errorf("row %d: %s / %s / %s\n  %s", i+2, row["artist"], row["title"], row["album"], strings.Join(diffs, "\n  "))
		}
	}
	t.Logf("%d of %d cases as expected", len(rows)-failed, len(rows))
}
