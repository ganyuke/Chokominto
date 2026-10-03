package web

import (
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"chokominto/internal/jobs"
	"chokominto/internal/resolve"
	"chokominto/internal/store"
)

func (e *env) sourceID(title, album string) int64 {
	e.t.Helper()
	var id int64
	if err := e.db.Reader().QueryRow(`SELECT id FROM sources WHERE title_text = ? AND album_text = ?`, title, album).Scan(&id); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// exactID finds a song or album by its exact name.
func (e *env) exactID(kind, name string) int64 {
	e.t.Helper()
	table := map[string]string{"song": "songs", "release": "releases"}[kind]
	var id int64
	if err := e.db.Reader().QueryRow(`SELECT id FROM `+table+` WHERE name = ? AND merged_into IS NULL`, name).Scan(&id); err != nil {
		e.t.Fatalf("no %s %q: %v", kind, name, err)
	}
	return id
}

func (e *env) albumOf(listen int64) int64 {
	e.t.Helper()
	var id int64
	e.db.Reader().QueryRow(`SELECT coalesce(release_id, 0) FROM listens WHERE id = ?`, listen).Scan(&id)
	return id
}

// follow posts a form and returns the page it leads to.
func (e *env) follow(path string, form url.Values) string {
	e.t.Helper()
	code, body, h := e.post(path, form)
	if code != http.StatusSeeOther {
		e.t.Fatalf("POST %s: %d\n%s", path, code, body)
	}
	_, body, _ = e.get(h.Get("Location"))
	return body
}

func TestScrobblesTab(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"YOASOBI", "Idol", "THE BOOK 3"},
		[3]string{"YOASOBI", "Idol", "THE BOOK 3"},
		[3]string{"YOASOBI", "Idol (TV size)", "Idol - Single"},
		[3]string{"YOASOBI", "Idol", "Idol - Single"},
		[3]string{"YOASOBI", "Gunjou", "THE BOOK"},
	)
	song := e.exactID("song", "Idol")
	book := e.exactID("release", "THE BOOK 3")
	single := e.exactID("release", "Idol - Single")
	path := fmt.Sprintf("/song/%d/scrobbles", song)

	if code, _, _ := e.get(path); code != http.StatusSeeOther {
		t.Fatalf("scrobbles tab logged out: %d", code)
	}
	if _, body, _ := e.get(fmt.Sprintf("/song/%d", song)); strings.Contains(body, "/scrobbles") {
		t.Fatal("Scrobbles tab shown to a visitor")
	}
	e.login()
	_, body, _ := e.get(fmt.Sprintf("/song/%d", song))
	if !strings.Contains(body, path+`">Scrobbles</a>`) || !strings.Contains(body, "Main version") {
		t.Fatalf("song page has no Scrobbles tab or names no main version:\n%s", body)
	}
	_, body, _ = e.get(path)
	for _, want := range []string{"Main version", "TV size", "Idol (TV size)", "Read automatically", ">Fix</a>", "Destination", "Move checked to this version"} {
		if !strings.Contains(body, want) {
			t.Fatalf("scrobbles tab lacks %q:\n%s", want, body)
		}
	}

	// The TV size text was really the main version.
	tv := e.sourceID("Idol (TV size)", "Idol - Single")
	main := e.recordingOf(e.listenID("Idol", "Idol - Single"))
	body = e.follow(path, url.Values{"s": {fmt.Sprint(tv)}, "do": {"version"}, "version": {fmt.Sprint(main)}})
	if !strings.Contains(body, "Moved 1 listen from Idol (TV size) to Idol.") || !strings.Contains(body, "By hand, ") {
		t.Fatalf("after moving to the main version:\n%s", body)
	}
	if got := e.recordingOf(e.listenID("Idol (TV size)", "Idol - Single")); got != main {
		t.Fatalf("on recording %d, want %d", got, main)
	}

	// Searching keeps the ticks, and the result can be picked.
	_, body, _ = e.get(path + "?s=" + fmt.Sprint(tv) + "&q=gunjou")
	gunjou := e.recordingOf(e.listenID("Gunjou", "THE BOOK"))
	if !strings.Contains(body, fmt.Sprintf(`value="%d" checked`, tv)) || !strings.Contains(body, fmt.Sprintf(`value="song:%d"`, gunjou)) {
		t.Fatalf("search lost the tick or found nothing:\n%s", body)
	}

	// Nothing ticked is refused, in words.
	code, _, h := e.post(path, url.Values{"do": {"no-album"}})
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "err=pick-rows") {
		t.Fatalf("nothing ticked: %d %s", code, h.Get("Location"))
	}

	// The album tab: both single texts go onto THE BOOK 3.
	albumPath := fmt.Sprintf("/album/%d/scrobbles", single)
	_, body, _ = e.get(albumPath + "?album=book+3")
	if !strings.Contains(body, fmt.Sprintf(`value="album:%d"`, book)) || !strings.Contains(body, "Received text") {
		t.Fatalf("album search:\n%s", body)
	}
	other := e.sourceID("Idol", "Idol - Single")
	body = e.follow(albumPath, url.Values{"s": {fmt.Sprint(tv), fmt.Sprint(other)}, "do": {fmt.Sprintf("album:%d", book)}})
	if !strings.Contains(body, "Put 2 listens on the album THE BOOK 3.") {
		t.Fatalf("after changing the album:\n%s", body)
	}
	if got := e.albumOf(e.listenID("Idol", "Idol - Single")); got != book {
		t.Fatalf("on album %d, want %d", got, book)
	}

	// Fixed links lists what was moved by hand, and can hand it back.
	_, body, _ = e.get("/settings/links")
	if !strings.Contains(body, "Idol (TV size)") || !strings.Contains(body, "2 in all") {
		t.Fatalf("fixed links:\n%s", body)
	}
	body = e.follow("/settings/links", url.Values{"s": {fmt.Sprint(tv)}, "do": {"reread"}})
	if !strings.Contains(body, "Read 1 received text again automatically.") || !strings.Contains(body, "1 in all") {
		t.Fatalf("after reading again:\n%s", body)
	}
	if got := e.recordingOf(e.listenID("Idol (TV size)", "Idol - Single")); got == main {
		t.Fatal("still on the main version after reading again")
	}
}

func TestFixScope(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"YOASOBI", "Idol", "THE BOOK 3"},
		[3]string{"YOASOBI", "Gunjou", "THE BOOK"},
		[3]string{"YOASOBI", "Idol", "THE BOOK 3"},
		[3]string{"YOASOBI", "Idol", "THE BOOK 3"},
		[3]string{"YOASOBI", "Idol", "Idol - Single"},
	)
	e.login()
	var listens []int64
	rows, _ := e.db.Reader().Query(`SELECT l.id FROM listens l JOIN sources s ON s.id = l.source_id WHERE s.title_text = 'Idol' AND s.album_text = 'THE BOOK 3' ORDER BY l.listened_at`)
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		listens = append(listens, id)
	}
	rows.Close()
	if len(listens) != 3 {
		t.Fatalf("%d Idol listens", len(listens))
	}
	one := listens[0]
	idol := e.recordingOf(one)
	gunjou := e.recordingOf(e.listenID("Gunjou", "THE BOOK"))
	fix := fmt.Sprintf("/listen/%d/fix", one)
	link := fmt.Sprintf("/listen/%d/link", one)
	on := func(want ...int64) {
		t.Helper()
		for i, w := range want {
			if got := e.recordingOf(listens[i]); got != w {
				t.Fatalf("listen %d is on %d, want %d", i, got, w)
			}
		}
	}
	undoAll := func(body string) {
		t.Helper()
		for _, m := range regexp.MustCompile(`action="/changes/(\d+)/undo"`).FindAllStringSubmatch(body, -1) {
			e.post("/changes/"+m[1]+"/undo", nil)
		}
		on(idol, idol, idol)
	}

	// sentLater is a new listen sent with the same text as the three.
	at := time.Now().Unix()
	sentLater := func() int64 {
		t.Helper()
		at++
		if _, err := e.db.InsertListens(t.Context(), e.userID, "listenbrainz", nil,
			[]store.NewListen{{ListenedAt: at, Artist: "YOASOBI", Title: "Idol", Album: "THE BOOK 3", Payload: []byte(`{}`)}}); err != nil {
			t.Fatal(err)
		}
		var id int64
		e.db.Reader().QueryRow(`SELECT id FROM listens WHERE listened_at = ?`, at).Scan(&id)
		if id == 0 {
			t.Fatal("the later listen wasn't stored")
		}
		return id
	}

	// Both choices are on the page before anything is done, in the same
	// words whatever the text's listen count.
	_, body, _ := e.get(fix + "?q=gunjou")
	for _, want := range []string{"This exact artist, title and album was received 3 times.",
		"Listens so far", "Only this listen", "Every listen sent with this text", "3 listens so far.",
		"Future scrobbles", "Leave future scrobbles alone", "Match exact title, artist and album in future scrobbles",
		"Match exact title and artist in future scrobbles (disregarding album)",
		"Match roughly the title and artist in future scrobbles (disregarding spacing, capitalization and album)",
		"Changes below are applied to every listen sent with this text so far, and to future scrobbles with the exact title, artist and album.",
		`name="scope" value="all"`, `name="later" value="text"`, "Move 3 listens here", "Read automatically", "Correction",
		"is deleted.", "The other 2 sent with the same text are not deleted."} {
		if !strings.Contains(body, want) {
			t.Fatalf("fix page lacks %q:\n%s", want, body)
		}
	}
	_, body, _ = e.get(fix + "?scope=one&later=no&q=gunjou")
	for _, want := range []string{"Changes below are applied to this listen. Future scrobbles are left alone.", "Move this listen here",
		`name="scope" value="one"`, `name="later" value="no"`, "The other 3 listens of Idol are not moved, and its names are not changed."} {
		if !strings.Contains(body, want) {
			t.Fatalf("fix page for one listen lacks %q:\n%s", want, body)
		}
	}
	// The choices keep each other: picking one never resets the other.
	if !strings.Contains(body, "/fix?scope=all&amp;later=no#scope") || !strings.Contains(body, "/fix?scope=one&amp;later=album#scope") {
		t.Fatalf("scope links drop the other choice:\n%s", body)
	}

	// 1. Only this listen, later ones not changed.
	body = e.follow(link, url.Values{"recording": {fmt.Sprint(gunjou)}, "scope": {"one"}, "later": {"no"}})
	if !strings.Contains(body, "Moved 1 listen from Idol to Gunjou.") || !strings.Contains(body, "This listen was fixed alone before") {
		t.Fatalf("one, not later:\n%s", body)
	}
	on(gunjou, idol, idol)
	if _, body, _ = e.get(fmt.Sprintf("/song/%d/scrobbles", e.exactID("song", "Gunjou"))); !strings.Contains(body, "Fixed alone") {
		t.Fatalf("scrobbles tab:\n%s", body)
	}
	if _, body, _ = e.get(fmt.Sprintf("/history?text=%d", e.sourceID("Idol", "THE BOOK 3"))); !strings.Contains(body, "Only the 3 listens sent as “Idol” by YOASOBI, on THE BOOK 3.") ||
		strings.Count(body, ">Fix</a>") != 3 {
		t.Fatalf("history for one text:\n%s", body)
	}
	body = e.follow(fmt.Sprintf("/listen/%d/follow", one), nil)
	if !strings.Contains(body, "Linked 1 listen like the others sent with the same text again.") {
		t.Fatalf("after following the text again:\n%s", body)
	}
	on(idol, idol, idol)

	// 2. Only this listen, and later ones with this text: the other two
	// stay, and a new listen sent the same way goes to Gunjou.
	body = e.follow(link, url.Values{"recording": {fmt.Sprint(gunjou)}, "scope": {"one"}, "later": {"text"}})
	if !strings.Contains(body, "Moved 1 listen from Idol to Gunjou, and later ones sent with the same text.") {
		t.Fatalf("one, and later:\n%s", body)
	}
	on(gunjou, idol, idol)
	newest := sentLater()
	if e.recordingOf(newest) != gunjou {
		t.Fatal("a later listen with the same text didn't follow")
	}
	e.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM listens WHERE id = ?`, newest); return err })
	undoAll(body)

	// 3. Every listen so far, later ones not changed.
	body = e.follow(link, url.Values{"recording": {fmt.Sprint(gunjou)}, "scope": {"all"}, "later": {"no"}})
	if !strings.Contains(body, "Moved 3 listens from Idol to Gunjou, but not later ones.") || strings.Contains(body, "Rules for similar listens") {
		t.Fatalf("all, not later:\n%s", body)
	}
	on(gunjou, gunjou, gunjou)
	newest = sentLater()
	if e.recordingOf(newest) != idol {
		t.Fatal("a later listen followed, though later ones weren't to change")
	}
	e.db.Write(t.Context(), func(tx *sql.Tx) error { _, err := tx.Exec(`DELETE FROM listens WHERE id = ?`, newest); return err })
	undoAll(body)

	// 4. Every listen so far and later ones, which offers wider rules after.
	body = e.follow(link, url.Values{"recording": {fmt.Sprint(gunjou)}, "scope": {"all"}, "later": {"text"}})
	if !strings.Contains(body, "Moved 3 listens from Idol to Gunjou.") || !strings.Contains(body, "Rules for similar listens") {
		t.Fatalf("all, and later:\n%s", body)
	}
	on(gunjou, gunjou, gunjou)
	undoAll(body)

	// A rule goes with either choice of listens: here only this listen
	// moves now, and the rule is saved for any album.
	body = e.follow(link, url.Values{"recording": {fmt.Sprint(gunjou)}, "scope": {"one"}, "later": {"album"}})
	if !strings.Contains(body, "Moved 1 listen from Idol to Gunjou, and later ones sent with the same text.") ||
		!strings.Contains(body, "Rule saved with the move: Always link “Idol” by YOASOBI here, whatever the album.") ||
		!strings.Contains(body, "Undo the rule") || strings.Contains(body, "whatever the album</td>") {
		t.Fatalf("one, with a rule:\n%s", body)
	}
	on(gunjou, idol, idol)
	// The rule is for future scrobbles only: the listen already received
	// with the other album is not moved, and a later one sent that way is
	// linked by the rule.
	single := e.listenID("Idol", "Idol - Single")
	r := &resolve.Resolver{DB: e.db}
	(&jobs.Runner{DB: e.db, Handlers: map[string]jobs.Handler{"resolve": r.Job, "reparse": r.ReparseJob}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}).Drain(t.Context())
	if e.recordingOf(single) != idol {
		t.Fatal("the rule moved a listen that was already received")
	}
	if _, err := e.db.InsertListens(t.Context(), e.userID, "listenbrainz", nil,
		[]store.NewListen{{ListenedAt: at + 100, Artist: "YOASOBI", Title: "Idol", Album: "Idol - Single", Payload: []byte(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	var future int64
	e.db.Reader().QueryRow(`SELECT id FROM listens WHERE listened_at = ?`, at+100).Scan(&future)
	if e.recordingOf(future) != gunjou {
		t.Fatal("a future scrobble on another album wasn't linked by the rule")
	}
	var rules int
	e.db.Reader().QueryRow(`SELECT count(*) FROM rules WHERE kind = 'link'`).Scan(&rules)
	if rules != 1 {
		t.Fatalf("%d link rules, want 1", rules)
	}
}

func TestFixAlbumAndCorrection(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"SomeChannel", "TV Size", ""},
		[3]string{"SomeChannel", "TV Size", ""},
		[3]string{"LiSA", "Gurenge", "LEO-NiNE"},
		[3]string{"LiSA", "Gurenge", "Gurenge - Single"},
	)
	e.login()
	wrong := e.listenID("TV Size", "")
	fix := fmt.Sprintf("/listen/%d", wrong)
	gurenge := e.exactID("song", "Gurenge")

	// Typing what should have been sent lands on the song, as a version.
	body := e.follow(fix+"/correct", url.Values{"artist": {"LiSA"}, "title": {"Gurenge (TV Size)"}, "album_text": {"LEO-NiNE"}})
	if !strings.Contains(body, "Moved 2 listens from TV Size to Gurenge (TV Size).") {
		t.Fatalf("after the correction:\n%s", body)
	}
	var song int64
	var version string
	e.db.Reader().QueryRow(`SELECT song_id, version FROM recordings WHERE id = ?`, e.recordingOf(wrong)).Scan(&song, &version)
	if song != gurenge || version != "TV Size" {
		t.Fatalf("on song %d version %q, want %d TV Size", song, version, gurenge)
	}
	leo := e.exactID("release", "LEO-NiNE")
	if e.albumOf(wrong) != leo {
		t.Fatal("not on the album typed")
	}
	// A text sent once reads the same as one sent many times.
	if _, once, _ := e.get(fmt.Sprintf("/listen/%d/fix", e.listenID("Gurenge", "LEO-NiNE"))); !strings.Contains(once, "Every listen sent with this text") ||
		!strings.Contains(once, "1 listen so far.") || !strings.Contains(once, "Changes below are applied to every listen sent with this text so far, and to future scrobbles with the exact title, artist and album.") {
		t.Fatalf("a text sent once:\n%s", once)
	}
	// The version table lists both, with this one current.
	if !strings.Contains(body, `<h2 id="version">Version</h2>`) || !strings.Contains(body, "<strong>Current</strong>") {
		t.Fatalf("no version table:\n%s", body)
	}
	// A correction saves the scope's rule too, like any other move.
	e.scrobbleNow([3]string{"AnotherChannel", "Opening", ""})
	op := e.listenID("Opening", "")
	ruled := e.follow(fmt.Sprintf("/listen/%d/correct", op), url.Values{"later": {"album"}, "artist": {"LiSA"}, "title": {"Gurenge"}, "album_text": {""}})
	if !strings.Contains(ruled, "Rule saved with the move: Always link “Opening” by AnotherChannel here, whatever the album.") {
		t.Fatalf("correction with a rule:\n%s", ruled)
	}
	// An empty correction is refused in words.
	if code, body, _ := e.post(fix+"/correct", url.Values{"artist": {""}, "title": {""}}); code != http.StatusBadRequest || !strings.Contains(body, "Type an artist and a title.") {
		t.Fatalf("empty correction: %d", code)
	}

	// The album alone can change too, or go.
	single := e.exactID("release", "Gurenge - Single")
	_, body, _ = e.get(fix + "/fix?album=single")
	if !strings.Contains(body, fmt.Sprintf(`name="to" value="%d"`, single)) {
		t.Fatalf("album search:\n%s", body)
	}
	body = e.follow(fix+"/album", url.Values{"to": {fmt.Sprint(single)}})
	if !strings.Contains(body, "Put 2 listens of Gurenge (TV Size) on the album Gurenge - Single.") || e.albumOf(wrong) != single {
		t.Fatalf("after changing the album:\n%s", body)
	}
	body = e.follow(fix+"/album", url.Values{"to": {"none"}})
	if !strings.Contains(body, "Took 2 listens of Gurenge (TV Size) off their album.") || e.albumOf(wrong) != 0 {
		t.Fatalf("after taking the album off:\n%s", body)
	}
	if code, _, _ := e.post(fix+"/album", url.Values{}); code != http.StatusBadRequest {
		t.Fatalf("no album picked: %d", code)
	}
}

func TestSongPictureAndAlbums(t *testing.T) {
	e := newEnv(t, true)
	e.scrobbleNow(
		[3]string{"Nanoka Hara", "Suzume", "Suzume"},
		[3]string{"Nanoka Hara", "Suzume", "Suzume (Motion Picture Soundtrack)"},
		[3]string{"Nanoka Hara", "Suzume", "Suzume (feat. Toaka)"},
		[3]string{"SomeChannel", "No album here", ""},
	)
	e.login()
	song := e.exactID("song", "Suzume")
	edit := fmt.Sprintf("/song/%d/edit", song)
	keep := e.exactID("release", "Suzume")
	ost := e.exactID("release", "Suzume (Motion Picture Soundtrack)")
	feat := e.exactID("release", "Suzume (feat. Toaka)")

	// The song page lists each album on a line of its own, and the Edit tab
	// can merge them.
	if _, body, _ := e.get(fmt.Sprintf("/song/%d", song)); !strings.Contains(body, `<th scope="row">Albums</th>`) || strings.Count(body, `<li><a href="/album/`) != 3 {
		t.Fatalf("song page albums:\n%s", body)
	}
	_, body, _ := e.get(edit)
	for _, want := range []string{`<h3 id="albums">Albums</h3>`, "Merge checked into", `<h3 id="picture">Picture</h3>`, "None of this song's albums has one either.",
		"3 listens, 1 version and 1 name"} {
		if !strings.Contains(body, want) {
			t.Fatalf("song edit lacks %q:\n%s", want, body)
		}
	}
	if code, _, h := e.post(edit, url.Values{"do": {"merge-albums"}, "into": {fmt.Sprint(keep)}}); code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "err=pick-albums") {
		t.Fatalf("no albums ticked: %d %s", code, h.Get("Location"))
	}
	body = e.follow(edit, url.Values{"do": {"merge-albums"}, "album": {fmt.Sprint(ost), fmt.Sprint(feat)}, "into": {fmt.Sprint(keep)}})
	if !strings.Contains(body, "Merged Suzume (Motion Picture Soundtrack), Suzume (feat. Toaka) into Suzume.") {
		t.Fatalf("after merging albums:\n%s", body)
	}
	if n := e.albumOf(e.listenID("Suzume", "Suzume (feat. Toaka)")); n != keep {
		t.Fatalf("listen on album %d, want %d", n, keep)
	}

	// The album page offers related albums both ways once there are some.
	e.scrobbleNow([3]string{"Nanoka Hara", "Suzume", "Suzume Single"})
	_, body, _ = e.get(fmt.Sprintf("/album/%d/edit", keep))
	single := e.exactID("release", "Suzume Single")
	if !strings.Contains(body, "Related albums") || !strings.Contains(body, "Merge into this album") {
		t.Fatalf("no related albums:\n%s", body)
	}
	body = e.follow(fmt.Sprintf("/album/%d/edit", keep), url.Values{"do": {"merge-here"}, "from": {fmt.Sprint(single)}})
	if !strings.Contains(body, "Merged Suzume Single into Suzume.") {
		t.Fatalf("after merging into this album:\n%s", body)
	}

	// A song with no album can have a picture of its own.
	lone := e.exactID("song", "No album here")
	path := fmt.Sprintf("/song/%d", lone)
	code, h := e.upload(path+"/picture", jpegBytes(t, 300, 300))
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), path+"/edit?done=") {
		t.Fatalf("upload: %d %s", code, h.Get("Location"))
	}
	_, body, _ = e.get(path + "/edit")
	if !strings.Contains(body, "This song's own picture.") || !strings.Contains(body, "Use the album's picture") {
		t.Fatalf("after upload:\n%s", body)
	}
	if _, body, _ = e.get(path); !strings.Contains(body, `<img class="cover" src="/art/`) {
		t.Fatalf("song page shows no cover:\n%s", body)
	}
	if _, body, _ = e.get("/history"); !strings.Contains(body, `<img src="/art/`) {
		t.Fatal("no thumbnail in History")
	}
	body = e.follow(path+"/edit", url.Values{"do": {"picture-remove"}})
	if !strings.Contains(body, "Took the picture off No album here.") {
		t.Fatalf("after removing the picture:\n%s", body)
	}
}
