package store

import (
	"cmp"
	"context"
	"database/sql"
	"slices"
	"time"
)

// Jobs are background work kept in the database, so a restart never drops
// them. A (kind, key) pair is queued at most once at a time.

type Job struct {
	ID       int64
	Kind     string
	Key      string
	Payload  string
	Attempts int
	lease    int64 // run_after while claimed. Anything else means it was queued again.
}

// jobLease is how long a claimed job is hidden from other workers. If the
// process dies mid-job, the job becomes due again after this.
const jobLease = 10 * time.Minute

// EnqueueTx queues a job inside an existing transaction. If the same job is
// already queued, it's made due no later than runAfter.
func EnqueueTx(ctx context.Context, tx *sql.Tx, kind, key, payload string, runAfter int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO jobs (kind, key, payload, run_after, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?)
		 ON CONFLICT (kind, key) DO UPDATE SET run_after = min(run_after, excluded.run_after)`,
		kind, key, payload, runAfter, unix())
	return err
}

func (db *DB) Enqueue(ctx context.Context, kind, key, payload string) error {
	return db.Write(ctx, func(tx *sql.Tx) error { return EnqueueTx(ctx, tx, kind, key, payload, 0) })
}

// ClaimJob takes the next due job of one of the given kinds, or returns
// ErrNotFound when none is due. Separate runners take separate kinds, so
// slow picture lookups never hold up linking new listens.
func (db *DB) ClaimJob(ctx context.Context, kinds []string) (Job, error) {
	js, err := db.ClaimJobs(ctx, kinds, 1)
	if err != nil {
		return Job{}, err
	}
	return js[0], nil
}

// ClaimJobs takes up to n due jobs of the given kinds, oldest due first,
// in one transaction. Claiming and finishing in batches saves two disk
// syncs per job, which is most of the time a job takes on an SD card.
func (db *DB) ClaimJobs(ctx context.Context, kinds []string, n int) ([]Job, error) {
	var out []Job
	err := db.Write(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		now := unix()
		var runAfter []int64
		// One query per kind, each straight from jobs_kind_due, then the
		// oldest of them. A single query over several kinds sorts every
		// due job of all of them.
		for _, kind := range kinds {
			rows, err := tx.QueryContext(ctx, `SELECT id, kind, key, payload, attempts, run_after FROM jobs
				WHERE kind = ? AND run_after <= ? ORDER BY run_after, id LIMIT ?`, kind, now, n)
			if err != nil {
				return err
			}
			for rows.Next() {
				var j Job
				var payload sql.NullString
				var ra int64
				if err := rows.Scan(&j.ID, &j.Kind, &j.Key, &payload, &j.Attempts, &ra); err != nil {
					rows.Close()
					return err
				}
				j.Payload = payload.String
				out = append(out, j)
				runAfter = append(runAfter, ra)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
		}
		if len(out) == 0 {
			return ErrNotFound
		}
		order := make([]int, len(out))
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, b int) int {
			return cmp.Or(cmp.Compare(runAfter[a], runAfter[b]), cmp.Compare(out[a].ID, out[b].ID))
		})
		picked := make([]Job, 0, n)
		for _, i := range order[:min(n, len(order))] {
			picked = append(picked, out[i])
		}
		out = picked
		lease := now + int64(jobLease/time.Second)
		for i := range out {
			out[i].Attempts++
			out[i].lease = lease
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET run_after = ?, attempts = ? WHERE id = ?`,
				lease, out[i].Attempts, out[i].ID); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// FinishJob removes a job that ran successfully. If it was queued again
// while running, its input changed, so it stays queued to run once more
// with a fresh count of attempts.
func (db *DB) FinishJob(ctx context.Context, j Job) error {
	return db.FinishJobs(ctx, []Job{j})
}

// FinishJobs is FinishJob for several jobs, in one transaction.
func (db *DB) FinishJobs(ctx context.Context, js []Job) error {
	if len(js) == 0 {
		return nil
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		for _, j := range js {
			res, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id = ? AND run_after = ?`, j.ID, j.lease)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				if _, err = tx.ExecContext(ctx, `UPDATE jobs SET attempts = 0, last_error = NULL WHERE id = ?`, j.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// ReleaseJobs hands claimed jobs that never ran back to the queue, due at
// once, as if they had never been claimed.
func (db *DB) ReleaseJobs(ctx context.Context, js []Job) error {
	if len(js) == 0 {
		return nil
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		for _, j := range js {
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET run_after = 0, attempts = attempts - 1 WHERE id = ? AND run_after = ?`,
				j.ID, j.lease); err != nil {
				return err
			}
		}
		return nil
	})
}

// RetryJob records a failure and makes the job due again after wait, or
// sooner if it was queued again while running.
func (db *DB) RetryJob(ctx context.Context, j Job, wait time.Duration, jobErr error) error {
	return db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs SET run_after = CASE WHEN run_after = ? THEN ? ELSE run_after END, last_error = ? WHERE id = ?`,
			j.lease, unix()+int64(wait/time.Second), jobErr.Error(), j.ID)
		return err
	})
}

// PendingJobs counts queued jobs of a kind.
func (db *DB) PendingJobs(ctx context.Context, kind string) (int, error) {
	var n int
	err := db.r.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE kind = ?`, kind).Scan(&n)
	return n, err
}
