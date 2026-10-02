package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// A job claimed by a process that then died is picked up again once its
// lease runs out.
func TestJobLeaseExpires(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	t0 := time.Unix(1_700_000_000, 0)
	now = func() time.Time { return t0 }
	t.Cleanup(func() { now = time.Now })

	if err := db.Enqueue(ctx, "x", "k", ""); err != nil {
		t.Fatal(err)
	}
	j, err := db.ClaimJob(ctx, []string{"x"})
	if err != nil || j.Attempts != 1 {
		t.Fatalf("claim: %+v %v", j, err)
	}
	if _, err := db.ClaimJob(ctx, []string{"x"}); err != ErrNotFound {
		t.Fatalf("claimed twice while leased: %v", err)
	}
	now = func() time.Time { return t0.Add(jobLease - time.Second) }
	if _, err := db.ClaimJob(ctx, []string{"x"}); err != ErrNotFound {
		t.Fatalf("claimed before the lease ran out: %v", err)
	}
	now = func() time.Time { return t0.Add(jobLease) }
	again, err := db.ClaimJob(ctx, []string{"x"})
	if err != nil || again.ID != j.ID || again.Attempts != 2 {
		t.Fatalf("after the lease: %+v %v", again, err)
	}
}

// A runner only takes the kinds of job it handles.
func TestClaimJobKinds(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	db.Enqueue(ctx, "artwork", "release:1:0", "")
	if _, err := db.ClaimJob(ctx, []string{"resolve"}); err != ErrNotFound {
		t.Fatalf("resolve runner took %v", err)
	}
	j, err := db.ClaimJob(ctx, []string{"artwork"})
	if err != nil || j.Kind != "artwork" {
		t.Fatalf("artwork runner: %+v %v", j, err)
	}
}

// A batch takes the oldest due jobs across all its kinds, and jobs handed
// back are due again with the attempt not counted.
func TestClaimJobsBatch(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	db.Write(ctx, func(tx *sql.Tx) error {
		for i, k := range []string{"a:1", "b:1", "a:2", "b:2", "a:3"} {
			kind, key, _ := strings.Cut(k, ":")
			if err := EnqueueTx(ctx, tx, kind, key, "", int64(10+i)); err != nil {
				return err
			}
		}
		return EnqueueTx(ctx, tx, "c", "1", "", 0)
	})
	js, err := db.ClaimJobs(ctx, []string{"a", "b"}, 3)
	var got []string
	for _, j := range js {
		got = append(got, j.Kind+":"+j.Key)
	}
	if err != nil || !slices.Equal(got, []string{"a:1", "b:1", "a:2"}) {
		t.Fatalf("batch: %v %v", got, err)
	}
	if err := db.ReleaseJobs(ctx, js[1:]); err != nil {
		t.Fatal(err)
	}
	if err := db.FinishJobs(ctx, js[:1]); err != nil {
		t.Fatal(err)
	}
	js, err = db.ClaimJobs(ctx, []string{"a", "b"}, 10)
	got = got[:0]
	for _, j := range js {
		got = append(got, fmt.Sprintf("%s:%s/%d", j.Kind, j.Key, j.Attempts))
	}
	if err != nil || !slices.Equal(got, []string{"b:1/1", "a:2/1", "b:2/1", "a:3/1"}) {
		t.Fatalf("after handing back: %v %v", got, err)
	}
}
