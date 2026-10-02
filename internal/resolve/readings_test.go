package resolve

import (
	"context"
	"strings"
	"testing"
)

func (e *env) artistsOf(title string) string {
	e.t.Helper()
	return strings.Join(e.q(`SELECT a.name FROM sources s JOIN recording_credits rc ON rc.recording_id = s.recording_id
		JOIN artists a ON a.id = rc.artist_id WHERE s.title_text = ? ORDER BY rc.position`, title), " | ")
}

func TestSwitchReadings(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.withReparse()
	e.scrobble("CHiCO, HoneyWorks", "ツーマンライブ", "")
	if got := e.artistsOf("ツーマンライブ"); got != "CHiCO | HoneyWorks" {
		t.Fatalf("with commas on: %s", got)
	}
	states, _, err := ReadingSettings(ctx, e.db, e.user)
	if err != nil {
		t.Fatal(err)
	}
	on := map[string]bool{}
	for _, s := range states {
		on[s.Key] = s.On
	}
	if !on["comma"] || !on["version"] || on["amp"] || on["bareslash"] {
		t.Fatalf("defaults %v", on)
	}

	edit, err := SetReadings(ctx, e.db, e.user, map[string]bool{"comma": false})
	if err != nil || edit == 0 {
		t.Fatal(edit, err)
	}
	e.run.Drain(ctx)
	if got := e.artistsOf("ツーマンライブ"); got != "CHiCO, HoneyWorks" {
		t.Fatalf("with commas off: %s", got)
	}
	// Switching it off again changes nothing.
	if again, err := SetReadings(ctx, e.db, e.user, map[string]bool{"comma": false}); err != nil || again != 0 {
		t.Fatalf("second switch off: %d %v", again, err)
	}
	if _, err := Undo(ctx, e.db, e.user, edit); err != nil {
		t.Fatal(err)
	}
	e.run.Drain(ctx)
	if got := e.artistsOf("ツーマンライブ"); got != "CHiCO | HoneyWorks" {
		t.Fatalf("after undo: %s", got)
	}

	// A reading that's off by default is added when switched on.
	if _, err := SetReadings(ctx, e.db, e.user, map[string]bool{"amp": true}); err != nil {
		t.Fatal(err)
	}
	e.scrobble("LiSA & Uru", "再会", "")
	if got := e.artistsOf("再会"); got != "LiSA | Uru" {
		t.Fatalf("with & on: %s", got)
	}
}

func TestOwnRules(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.withReparse()
	e.scrobble("YOASOBI", "アイドル", "")
	e.scrobble("YOASOBI", "Idol", "")
	idol := e.id(`SELECT recording_id FROM sources WHERE title_text = 'アイドル'`)
	en := e.sourceOf("YOASOBI", "Idol", "")
	if _, err := e.db.LinkSources(ctx, e.user, []int64{en}, idol); err != nil {
		t.Fatal(err)
	}
	offers, _ := Offers(ctx, e.db, e.user, en, idol)
	if _, err := SaveOffer(ctx, e.db, e.user, offers[0], "x"); err != nil {
		t.Fatal(err)
	}
	_, own, err := ReadingSettings(ctx, e.db, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 || own[0].Text != "Always link “Idol” by YOASOBI to アイドル, whatever the album" || !own[0].On {
		t.Fatalf("own rules %+v", own)
	}
	if _, err := SetOwnRule(ctx, e.db, e.user, own[0].ID, "off"); err != nil {
		t.Fatal(err)
	}
	if _, own, _ = ReadingSettings(ctx, e.db, e.user); own[0].On {
		t.Fatal("still on")
	}
	del, err := SetOwnRule(ctx, e.db, e.user, own[0].ID, "delete")
	if err != nil {
		t.Fatal(err)
	}
	if _, own, _ = ReadingSettings(ctx, e.db, e.user); len(own) != 0 {
		t.Fatal("not deleted")
	}
	if _, err := Undo(ctx, e.db, e.user, del); err != nil {
		t.Fatal(err)
	}
	if _, own, _ = ReadingSettings(ctx, e.db, e.user); len(own) != 1 {
		t.Fatal("undo didn't bring it back")
	}
	// Built-in readings aren't own rules.
	if _, err := SetOwnRule(ctx, e.db, e.user, 1, "delete"); err == nil {
		t.Fatal("deleted a built-in reading as an own rule")
	}
}
