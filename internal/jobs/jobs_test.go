package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"chokominto/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "j.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type jobRow struct {
	runAfter, attempts int64
	lastError          string
}

func jobState(t *testing.T, db *store.DB, key string) (jobRow, bool) {
	t.Helper()
	var r jobRow
	err := db.Reader().QueryRow(`SELECT run_after, attempts, coalesce(last_error, '') FROM jobs WHERE key = ?`, key).
		Scan(&r.runAfter, &r.attempts, &r.lastError)
	if err != nil {
		return r, false
	}
	return r, true
}

func TestFailuresBackOff(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	db.Enqueue(ctx, "x", "k", "")
	calls := 0
	r := &Runner{DB: db, Log: quiet(), Handlers: map[string]Handler{"x": func(context.Context, store.Job) error {
		calls++
		return errors.New("no network")
	}}}
	start := time.Now().Unix()
	if !r.RunOne(ctx) {
		t.Fatal("job not run")
	}
	j, ok := jobState(t, db, "k")
	if !ok || j.attempts != 1 || j.lastError != "no network" {
		t.Fatalf("after a failure: %+v %v", j, ok)
	}
	if wait := j.runAfter - start; wait < 59 || wait > 61 {
		t.Fatalf("retry in %ds, want a minute", wait)
	}
	// Not due again yet.
	if r.RunOne(ctx) || calls != 1 {
		t.Fatalf("retried at once, calls = %d", calls)
	}
	for n, want := range []time.Duration{time.Minute, 10 * time.Minute, time.Hour, 6 * time.Hour, 6 * time.Hour} {
		if got := backoff(n + 1); got != want {
			t.Errorf("backoff(%d) = %v, want %v", n+1, got, want)
		}
	}
}

// A panic is retried like an error. A kind the runner has no handler for
// is left queued for the runner that has one.
func TestPanicsAndUnknownKindsAreRetried(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	db.Enqueue(ctx, "boom", "p", "")
	db.Enqueue(ctx, "nobody", "u", "")
	r := &Runner{DB: db, Log: quiet(), Handlers: map[string]Handler{"boom": func(context.Context, store.Job) error {
		panic("bad data")
	}}}
	r.Drain(ctx)
	if j, ok := jobState(t, db, "p"); !ok || j.lastError != "panic: bad data" {
		t.Errorf("p: %+v %v", j, ok)
	}
	if j, ok := jobState(t, db, "u"); !ok || j.attempts != 0 || j.lastError != "" {
		t.Errorf("another runner's job was touched: %+v %v", j, ok)
	}
}

func TestWorkersNeverShareAJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	db := openDB(t)
	const n = 200
	for i := range n {
		if err := db.Enqueue(ctx, "x", strconv.Itoa(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	runs := map[string]int{}
	r := &Runner{DB: db, Log: quiet(), Workers: 4, Idle: time.Millisecond, Handlers: map[string]Handler{"x": func(_ context.Context, j store.Job) error {
		mu.Lock()
		runs[j.Key]++
		mu.Unlock()
		return nil
	}}}
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		left, _ := db.PendingJobs(context.Background(), "x")
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d jobs left", left)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if len(runs) != n {
		t.Fatalf("ran %d distinct jobs, want %d", len(runs), n)
	}
	for k, c := range runs {
		if c != 1 {
			t.Errorf("job %s ran %d times", k, c)
		}
	}
}

// Queueing a job again while it runs means its input changed, so it has
// to run once more afterwards.
func TestRequeuedWhileRunningRunsAgain(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	db.Enqueue(ctx, "x", "k", "")
	calls := 0
	r := &Runner{DB: db, Log: quiet(), Handlers: map[string]Handler{"x": func(context.Context, store.Job) error {
		calls++
		if calls == 1 {
			return db.Enqueue(ctx, "x", "k", "")
		}
		return nil
	}}}
	r.Drain(ctx)
	if calls != 2 {
		t.Fatalf("ran %d times, want 2", calls)
	}
	if _, ok := jobState(t, db, "k"); ok {
		t.Fatal("job left in the queue")
	}
}

func TestRequeuedWhileFailingSkipsBackoff(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	db.Enqueue(ctx, "x", "k", "")
	calls := 0
	r := &Runner{DB: db, Log: quiet(), Handlers: map[string]Handler{"x": func(context.Context, store.Job) error {
		calls++
		if calls == 1 {
			db.Enqueue(ctx, "x", "k", "")
			return errors.New("stale input")
		}
		return nil
	}}}
	r.Drain(ctx)
	if calls != 2 {
		t.Fatalf("ran %d times, want 2 (the second without waiting)", calls)
	}
}
