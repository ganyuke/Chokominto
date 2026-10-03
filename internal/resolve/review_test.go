package resolve

import (
	"context"
	"testing"
)

func TestReviewSections(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Star Citizen")
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "Other Game")
	src := e.sourceOf("Pedro Macedo Camacho", "Main Theme", "Other Game")
	if _, _, err := NewSong(ctx, e.db, e.user, src, 0); err != nil {
		t.Fatal(err)
	}
	// Now there are two, and this one names no album.
	e.scrobble("Pedro Macedo Camacho", "Main Theme", "")
	e.scrobble("", "no artist sent", "")

	rt, err := Review(ctx, e.db, e.user)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.WhichOne) != 1 || len(rt.WhichOne[0].Candidates) != 2 || rt.WhichOne[0].Album != "" {
		t.Fatalf("which one: %+v", rt.WhichOne)
	}
	if len(rt.Incomplete) != 1 || rt.Incomplete[0].Title != "no artist sent" {
		t.Fatalf("incomplete: %+v", rt.Incomplete)
	}
	if len(rt.Unlinked) != 0 {
		t.Fatalf("unlinked: %+v", rt.Unlinked)
	}
	// Looking never creates anything.
	before := e.one(`SELECT count(*) FROM artists`)
	Review(ctx, e.db, e.user)
	if e.one(`SELECT count(*) FROM artists`) != before {
		t.Fatal("review created artists")
	}
}
