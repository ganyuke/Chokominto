package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
)

// Agent tasks group an agent's edits, so a whole batch can be undone at
// once, all or nothing. See docs/architecture.md, "Agents (MCP)".

// taskIdle is how long an unnamed task stays open without changes. After
// that, the agent's next change starts a new one.
const taskIdle = 30 * 60

type taskKey struct{}

// taskCtx says which task edits made with a context join.
type taskCtx struct {
	agent  bool  // the agent's open task, started when needed
	taskID int64 // this task, for undoing one
}

// InAgentTask makes every edit made with ctx join the agent's open task,
// starting one when there's none. The agent is the token set by WithAgent,
// or one started with chokominto mcp when there's none.
func InAgentTask(ctx context.Context) context.Context {
	return context.WithValue(ctx, taskKey{}, taskCtx{agent: true})
}

func inTask(ctx context.Context, taskID int64) context.Context {
	return context.WithValue(ctx, taskKey{}, taskCtx{taskID: taskID})
}

// editTaskTx returns the task an edit made with ctx joins, or nil.
func editTaskTx(ctx context.Context, tx *sql.Tx, userID int64, m EditMeta) (any, error) {
	t, ok := ctx.Value(taskKey{}).(taskCtx)
	if !ok || m.Automatic {
		return nil, nil
	}
	if t.taskID != 0 {
		return t.taskID, nil
	}
	now := unix()
	id, err := openTaskTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if id != 0 {
		_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET last_at = ? WHERE id = ?`, now, id)
		return id, err
	}
	return startTaskTx(ctx, tx, userID, "")
}

// agentToken is the token of the agent making edits with ctx, or NULL.
func agentToken(ctx context.Context) any {
	agent, _ := ctx.Value(agentKey{}).(int64)
	return nullID(agent)
}

// openTaskTx returns the agent's open task, or 0. An unnamed one left
// idle has run out, and is ended.
func openTaskTx(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	var id, last int64
	var name string
	err := tx.QueryRowContext(ctx, `SELECT id, name, last_at FROM agent_tasks
		WHERE user_id = ? AND agent_token_id IS ? AND ended_at IS NULL ORDER BY id DESC LIMIT 1`, userID, agentToken(ctx)).Scan(&id, &name, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if name == "" && unix()-last > taskIdle {
		_, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET ended_at = ? WHERE id = ?`, last, id)
		return 0, err
	}
	return id, nil
}

func startTaskTx(ctx context.Context, tx *sql.Tx, userID int64, name string) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET ended_at = ? WHERE user_id = ? AND agent_token_id IS ? AND ended_at IS NULL`,
		unix(), userID, agentToken(ctx)); err != nil {
		return 0, err
	}
	var id int64
	now := unix()
	err := tx.QueryRowContext(ctx, `INSERT INTO agent_tasks (user_id, agent_token_id, name, created_at, last_at) VALUES (?, ?, ?, ?, ?) RETURNING id`,
		userID, agentToken(ctx), name, now, now).Scan(&id)
	return id, err
}

// StartTask starts a named task for the agent making edits with ctx. Its
// edits from now on join it, until it finishes or another starts.
func (db *DB) StartTask(ctx context.Context, userID int64, name string) (int64, error) {
	name, err := cleanName(name)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.Write(ctx, func(tx *sql.Tx) error {
		id, err = startTaskTx(ctx, tx, userID, name)
		return err
	})
	return id, err
}

// FinishTask ends the agent's open task. It returns the task, or 0 when
// none was open.
func (db *DB) FinishTask(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var err error
		if id, err = openTaskTx(ctx, tx, userID); err != nil || id == 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET ended_at = ? WHERE id = ?`, unix(), id)
		return err
	})
	return id, err
}

// Task is a group of an agent's edits.
type Task struct {
	ID        int64
	Name      string // "" when the agent didn't name it
	Agent     string // the agent token's label
	Edits     int    // edits in it, not counting undos of its own edits
	Newest    int64  // its newest edit, undos too
	UndoneAt  sql.NullInt64
	CreatedAt int64
}

// Tasks looks up tasks by id.
func (db *DB) Tasks(ctx context.Context, userID int64, ids []int64) (map[int64]Task, error) {
	out := map[int64]Task{}
	for _, id := range slices.Compact(slices.Sorted(slices.Values(ids))) {
		var t Task
		err := db.r.QueryRowContext(ctx, `SELECT k.id, k.name, coalesce(tok.label, ''), k.undone_at, k.created_at,
				(SELECT count(*) FROM edits e WHERE task_id = k.id AND `+ownEdits+`), (SELECT max(id) FROM edits WHERE task_id = k.id)
			FROM agent_tasks k LEFT JOIN api_tokens tok ON tok.id = k.agent_token_id WHERE k.id = ? AND k.user_id = ?`, id, userID).
			Scan(&t.ID, &t.Name, &t.Agent, &t.UndoneAt, &t.CreatedAt, &t.Edits, &t.Newest)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, nil
}

// ownEdits leaves out undos of a task's own edits, which show as undone.
const ownEdits = `NOT (e.kind = 'undo' AND e.undoes IN (SELECT id FROM edits WHERE task_id = e.task_id))`

// TaskEdits lists a task's edits, newest first, leaving out undos of its
// own edits.
func (db *DB) TaskEdits(ctx context.Context, userID, taskID int64) ([]Edit, error) {
	return db.scanEdits(ctx, `WHERE e.user_id = ? AND e.task_id = ? AND `+ownEdits+` ORDER BY e.id DESC`, userID, taskID)
}

// TaskUndoEdits returns the edits UndoTaskTx would undo, newest first: the
// task's edits in effect, leaving out undos of its own edits, which are
// already out of effect. Undoing an undo of an older edit brings that edit
// back, as it was before the task.
func TaskUndoEdits(ctx context.Context, tx *sql.Tx, userID, taskID int64) ([]int64, error) {
	var undone sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT undone_at FROM agent_tasks WHERE id = ? AND user_id = ?`, taskID, userID).Scan(&undone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if undone.Valid {
		return nil, ErrAlreadyUndone
	}
	return ids(ctx, tx, `SELECT id FROM edits e WHERE task_id = ? AND undone_at IS NULL AND `+ownEdits+` ORDER BY id DESC`, taskID)
}

// UndoTaskTx undoes every edit of a task still in effect, newest first, as
// part of tx. If a later edit outside the task changed the same things, it
// returns a *ConflictError naming those, and the caller rolls tx back so
// nothing is undone. The undos join the task, which is marked undone.
func UndoTaskTx(ctx context.Context, tx *sql.Tx, userID, taskID int64, edits []int64) (int, error) {
	var before int64
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM edits`).Scan(&before); err != nil {
		return 0, err
	}
	ctx = inTask(ctx, taskID)
	for _, id := range edits {
		if _, err := UndoEditTx(ctx, tx, userID, id); err != nil {
			var conflict *ConflictError
			if errors.As(err, &conflict) {
				// Edits of the task, and the undos just made, aren't in the way.
				inside, err := ids(ctx, tx, `SELECT id FROM edits WHERE task_id = ?`, taskID)
				if err != nil {
					return 0, err
				}
				conflict.Later = slices.DeleteFunc(conflict.Later, func(l int64) bool { return l > before || slices.Contains(inside, l) })
				if len(conflict.Later) == 0 {
					return 0, ErrStale
				}
			}
			return 0, err
		}
	}
	t := unix()
	_, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET undone_at = ?, ended_at = coalesce(ended_at, ?) WHERE id = ?`, t, t, taskID)
	return len(edits), err
}
