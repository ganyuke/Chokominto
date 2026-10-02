package ingest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"chokominto/internal/store"
)

func TestIncomplete(t *testing.T) {
	ref := time.Unix(1_800_000_000, 0)
	ok := Listen{ListenedAt: 1_700_000_000, HasTime: true, Artist: "YOASOBI", Title: "アイドル"}
	cases := map[string]func(*Listen){
		"no time":      func(l *Listen) { l.HasTime = false },
		"empty artist": func(l *Listen) { l.Artist = "" },
		"blank title":  func(l *Listen) { l.Title = "  " },
		"before 2002":  func(l *Listen) { l.ListenedAt = 1000 },
		"far future":   func(l *Listen) { l.ListenedAt = ref.Add(25 * time.Hour).Unix() },
	}
	if Incomplete(ok, ref) {
		t.Fatal("complete listen flagged")
	}
	for name, mod := range cases {
		l := ok
		mod(&l)
		if !Incomplete(l, ref) {
			t.Errorf("%s not flagged", name)
		}
	}
	// No album is fine, and a little clock skew is fine.
	l := ok
	l.Album = ""
	l.ListenedAt = ref.Add(time.Hour).Unix()
	if Incomplete(l, ref) {
		t.Error("missing album or small skew flagged")
	}
}

func TestStoreChunksAndFlags(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "t.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u, _ := db.CreateUser(ctx, "elaina", "h")

	var ls []Listen
	for i := range ChunkSize + 10 {
		ls = append(ls, Listen{ListenedAt: int64(1_700_000_000 + i), HasTime: true, Artist: "A", Title: "T", Payload: []byte(`{}`)})
	}
	ls = append(ls, Listen{Artist: "", Title: "No artist", HasTime: true, ListenedAt: 1_700_000_000, Payload: []byte(`{}`)})
	res, err := Store(ctx, db, u, "import:maloja", nil, ls)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stored != len(ls) || res.Duplicates != 0 {
		t.Fatalf("%+v", res)
	}
	res, _ = Store(ctx, db, u, "import:maloja", nil, ls)
	if res.Stored != 0 || res.Duplicates != len(ls) {
		t.Fatalf("re-run %+v", res)
	}
	var flagged int
	db.Reader().QueryRow(`SELECT count(*) FROM listens WHERE incomplete = 1`).Scan(&flagged)
	if flagged != 1 {
		t.Fatalf("flagged = %d", flagged)
	}
}
