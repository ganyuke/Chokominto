package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func undoTask(t *testing.T, db *DB, user, task int64) (int, error) {
	t.Helper()
	var n int
	err := db.Write(context.Background(), func(tx *sql.Tx) error {
		edits, err := TaskUndoEdits(context.Background(), tx, user, task)
		if err != nil {
			return err
		}
		n, err = UndoTaskTx(context.Background(), tx, user, task, edits)
		return err
	})
	return n, err
}

func editTask(t *testing.T, db *DB, edit int64) int64 {
	t.Helper()
	return int64(count(t, db, `SELECT coalesce(task_id, 0) FROM edits WHERE id = ?`, edit))
}

func TestAgentTasks(t *testing.T) {
	owner := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	token, _ := db.CreateAgentToken(owner, m.u, "Claude", []byte("h"), true)
	agent := InAgentTask(WithAgent(owner, token))
	must := func(id int64, err error) int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	// Without a task named, one starts by itself and keeps going.
	a := must(db.AddName(agent, m.u, "artist", m.band, "Kessoku Band"))
	b := must(db.Merge(agent, m.u, "artist", m.yoasobiFW, m.yoasobi))
	auto := editTask(t, db, a)
	if auto == 0 || editTask(t, db, b) != auto {
		t.Fatalf("tasks %d %d", auto, editTask(t, db, b))
	}
	// The owner's own edits are never in one.
	if o := must(db.AddName(owner, m.u, "artist", m.ayase, "あやせ")); editTask(t, db, o) != 0 {
		t.Fatal("owner edit in a task")
	}

	// A named task, with an undo of its own change inside.
	named := must(db.StartTask(agent, m.u, "Tidy アイドル"))
	c := must(db.Rename(agent, m.u, "song", int64(count(t, db, `SELECT song_id FROM recordings WHERE id = ?`, m.idolJP)), "Aidoru"))
	d := must(db.AddName(agent, m.u, "release", m.single, "Idol (single)"))
	must(db.UndoEdit(agent, m.u, d))
	if editTask(t, db, c) != named || editTask(t, db, d) != named {
		t.Fatal("not in the named task")
	}
	if tasks, _ := db.Tasks(owner, m.u, []int64{auto, named}); tasks[named].Name != "Tidy アイドル" || tasks[named].Agent != "Claude" || tasks[named].Edits != 2 {
		t.Fatalf("tasks %+v", tasks)
	}

	// The owner changed one of the first task's names since, so undoing
	// it all is refused, naming that change, and nothing is undone.
	blocker := must(db.Rename(owner, m.u, "artist", m.band, "Kessoku Band!"))
	_, err := undoTask(t, db, m.u, auto)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || len(conflict.Later) != 1 || conflict.Later[0] != blocker {
		t.Fatalf("conflict: %v", err)
	}
	if n := count(t, db, `SELECT count(*) FROM edits WHERE undone_at IS NOT NULL AND task_id = ?`, auto); n != 0 {
		t.Fatalf("%d undone anyway", n)
	}
	must(db.UndoEdit(owner, m.u, blocker))
	if n, err := undoTask(t, db, m.u, auto); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	var merged sql.NullInt64
	db.r.QueryRow(`SELECT merged_into FROM artists WHERE id = ?`, m.yoasobiFW).Scan(&merged)
	if merged.Valid || count(t, db, `SELECT count(*) FROM artist_aliases WHERE name = 'Kessoku Band'`) != 0 {
		t.Fatal("first task not undone")
	}
	if _, err := undoTask(t, db, m.u, auto); !errors.Is(err, ErrAlreadyUndone) {
		t.Fatal(err)
	}

	// The named task: its own undo is left alone, so the album name stays
	// gone and the song gets its name back.
	if n, err := undoTask(t, db, m.u, named); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if count(t, db, `SELECT count(*) FROM release_aliases WHERE name = 'Idol (single)'`) != 0 ||
		count(t, db, `SELECT count(*) FROM songs WHERE name = 'Aidoru'`) != 0 {
		t.Fatal("named task not undone")
	}
	// The undos are in the task, but only its own changes are counted.
	if tasks, _ := db.Tasks(owner, m.u, []int64{named}); !tasks[named].UndoneAt.Valid || tasks[named].Edits != 2 {
		t.Fatalf("after undo %+v", tasks[named])
	}
}

func TestAgentTaskIdle(t *testing.T) {
	ctx := t.Context()
	db := openTest(t)
	m := newMusic(t, db)
	agent := InAgentTask(ctx) // chokominto mcp, no token
	a, _ := db.AddName(agent, m.u, "artist", m.band, "Kessoku Band")
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE agent_tasks SET last_at = last_at - 31*60`)
		return err
	})
	b, _ := db.AddName(agent, m.u, "artist", m.band, "Kessoku")
	if editTask(t, db, a) == 0 || editTask(t, db, b) == editTask(t, db, a) {
		t.Fatalf("idle task kept: %d %d", editTask(t, db, a), editTask(t, db, b))
	}
	// A named one doesn't run out.
	named, _ := db.StartTask(agent, m.u, "Names")
	db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE agent_tasks SET last_at = last_at - 3600`)
		return err
	})
	c, _ := db.AddName(agent, m.u, "artist", m.band, "KB")
	if editTask(t, db, c) != named {
		t.Fatal("named task ran out")
	}
	if id, _ := db.FinishTask(agent, m.u); id != named {
		t.Fatal("finish", id)
	}
	d, _ := db.AddName(agent, m.u, "artist", m.band, "K")
	if editTask(t, db, d) == named {
		t.Fatal("finished task kept going")
	}
}
