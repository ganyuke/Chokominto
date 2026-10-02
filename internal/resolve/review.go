package resolve

import (
	"context"
	"database/sql"
	"strings"

	"chokominto/internal/store"
)

// ReviewText sorts unlinked text into Review's sections.
type ReviewText struct {
	WhichOne   []Choice
	Unlinked   []store.UnlinkedSource
	Incomplete []store.UnlinkedSource
}

// Choice is text that fits several recordings, with the ones it fits.
type Choice struct {
	store.UnlinkedSource
	Candidates []int64
}

// Review reads the text the owner still has to place. It never creates or
// links anything: artists are only looked up.
func Review(ctx context.Context, db *store.DB, userID int64) (ReviewText, error) {
	var out ReviewText
	srcs, err := db.UnlinkedSources(ctx, userID)
	if err != nil {
		return out, err
	}
	err = db.ReadTx(ctx, func(tx *sql.Tx) error {
		rules, err := store.RulesTx(ctx, tx, userID)
		if err != nil {
			return err
		}
		for _, s := range srcs {
			t := Clean(Text{s.Artist, s.Title, s.Album}, rules)
			p := Parse(t.Artist, t.Title, rules)
			if s.Incomplete || strings.TrimSpace(t.Artist) == "" || p.Title == "" || len(p.Credits) == 0 {
				out.Incomplete = append(out.Incomplete, s)
				continue
			}
			if s.RecordingID.Valid {
				continue
			}
			var mains []string
			for _, c := range p.Credits {
				if c.Role == "main" {
					mains = append(mains, c.Name)
				}
			}
			if len(mains) == 0 {
				mains = []string{p.Credits[0].Name}
			}
			ids, err := store.FindArtistsTx(ctx, tx, userID, mains)
			if err != nil {
				return err
			}
			cands, err := store.FindRecordingsTx(ctx, tx, userID, p.Title, ids)
			if err != nil {
				return err
			}
			if len(cands) > 1 {
				out.WhichOne = append(out.WhichOne, Choice{s, cands})
			} else {
				out.Unlinked = append(out.Unlinked, s)
			}
		}
		return nil
	})
	return out, err
}
