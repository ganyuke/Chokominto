package resolve

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"chokominto/internal/store"
)

func TestRankings(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range 3 {
		e.scrobble("supercell", "World Is Mine", "supercell")
	}
	for range 5 {
		e.scrobble("Alya", "World Is Mine", "")
	}
	for range 4 {
		e.scrobble("後藤ひとり(CV:青山吉能)", "ひとりぼっち東京", "結束バンド")
	}
	e.scrobble("青山吉能", "Solo", "")
	all := store.RankQuery{From: 0, To: 1 << 40, Limit: 50}

	// Separate recordings: Alya's cover leads.
	songs, err := e.db.TopSongs(ctx, e.user, all, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range songs {
		got = append(got, fmt.Sprintf("%d %s/%s %d", s.Rank, s.Name, store.JoinNames(s.Artists), s.Listens))
	}
	if strings.Join(got, "; ") != "1 World Is Mine/Alya 5; 2 ひとりぼっち東京/後藤ひとり 4; 3 World Is Mine/supercell 3; 4 Solo/青山吉能 1" {
		t.Fatal(got)
	}

	// Covers combined only after the owner says they're the same song
	// (milestone 3). Here: merge by hand and check the combined row.
	e.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE recordings SET song_id = (SELECT min(song_id) FROM recordings r JOIN songs s ON s.id = r.song_id WHERE s.name = 'World Is Mine') WHERE song_id IN (SELECT id FROM songs WHERE name = 'World Is Mine')`)
		return err
	})
	songs, _ = e.db.TopSongs(ctx, e.user, all, true)
	if songs[0].Name != "World Is Mine" || songs[0].Listens != 8 || store.JoinNames(songs[0].Artists) != "supercell" {
		t.Fatalf("combined: %+v", songs[0])
	}

	// Artists: the voice actor gets the character's listens as credited.
	artists, _ := e.db.TopArtists(ctx, e.user, all, store.ByTotal)
	got = nil
	for _, a := range artists {
		got = append(got, fmt.Sprintf("%d %s %d/%d/%d", a.Rank, a.Artist.Name, a.Total, a.Credited, a.ViaGroups))
	}
	if strings.Join(got, "; ") != "1 Alya 5/5/0; 1 青山吉能 5/5/0; 3 後藤ひとり 4/4/0; 4 supercell 3/3/0" {
		t.Fatal(got)
	}

	// Hiding the Character label drops the character, not the voice actor.
	labels, _ := e.db.LabelsInUse(ctx, e.user, "artist")
	if len(labels) != 1 || labels[0].Name != "Character" || !labels[0].HideDefault {
		t.Fatalf("labels %+v", labels)
	}
	q := all
	q.HideLabels = []int64{labels[0].ID}
	artists, _ = e.db.TopArtists(ctx, e.user, q, store.ByTotal)
	for _, a := range artists {
		if a.Artist.Name == "後藤ひとり" {
			t.Fatal("character shown while hidden")
		}
	}

	// Albums.
	albums, _ := e.db.TopAlbums(ctx, e.user, all)
	if len(albums) != 2 || albums[0].Album.Name != "結束バンド" || albums[0].Listens != 4 {
		t.Fatalf("albums %+v", albums)
	}

	// A period that holds nothing.
	empty, _ := e.db.TopSongs(ctx, e.user, store.RankQuery{From: 0, To: 1, Limit: 50}, false)
	if len(empty) != 0 {
		t.Fatal("songs outside the period")
	}
}
