package main

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Feedback is a complaint or suggestion a sitter sends from the app. The owner reads it in the
// database and writes a reply there (see DEPLOY.md「读投诉与建议」); the sitter sees the reply in
// the app.
type Feedback struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Content   string `json:"content"`
	Contact   string `json:"contact"`
	Reply     string `json:"reply"`
	CreatedAt int64  `json:"created_at"`
}

var feedbackKinds = map[string]bool{"suggest": true, "complaint": true, "bug": true}

const (
	maxFeedbackRunes  = 500
	maxContactRunes   = 60
	maxFeedbackPerDay = 10
)

func (st *Store) AddFeedback(ctx context.Context, sitterID int64, f Feedback) (Feedback, error) {
	f.CreatedAt = st.now().Unix()
	res, err := st.db.ExecContext(ctx, `INSERT INTO feedback (sitter_id, kind, content, contact, created_at) VALUES (?, ?, ?, ?, ?)`,
		sitterID, f.Kind, f.Content, f.Contact, f.CreatedAt)
	if err != nil {
		return f, err
	}
	f.ID, err = res.LastInsertId()
	return f, err
}

// ListFeedback returns the sitter's own feedback, newest first.
func (st *Store) ListFeedback(ctx context.Context, sitterID int64) ([]Feedback, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT id, kind, content, contact, COALESCE(reply, ''), created_at FROM feedback
		WHERE sitter_id = ? ORDER BY created_at DESC, id DESC LIMIT 50`, sitterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Feedback{}
	for rows.Next() {
		var f Feedback
		if err := rows.Scan(&f.ID, &f.Kind, &f.Content, &f.Contact, &f.Reply, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (st *Store) countFeedbackSince(ctx context.Context, sitterID, since int64) (int, error) {
	var n int
	err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM feedback WHERE sitter_id = ? AND created_at >= ?`, sitterID, since).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}

func (a *API) postFeedback(w http.ResponseWriter, r *http.Request, s Sitter) {
	var in Feedback
	if !readJSON(w, r, &in) {
		return
	}
	in.Content, in.Contact = strings.TrimSpace(in.Content), strings.TrimSpace(in.Contact)
	switch {
	case !feedbackKinds[in.Kind]:
		writeErr(w, http.StatusBadRequest, "请选择是建议、投诉还是问题")
		return
	case in.Content == "":
		writeErr(w, http.StatusBadRequest, "写点内容再提交")
		return
	case utf8.RuneCountInString(in.Content) > maxFeedbackRunes:
		writeErr(w, http.StatusBadRequest, "内容最多 500 字")
		return
	case utf8.RuneCountInString(in.Contact) > maxContactRunes:
		writeErr(w, http.StatusBadRequest, "联系方式最多 60 字")
		return
	}
	n, err := a.Store.countFeedbackSince(r.Context(), s.ID, a.Store.now().Add(-24*time.Hour).Unix())
	if err != nil {
		a.fail(w, err)
		return
	}
	if n >= maxFeedbackPerDay {
		writeErr(w, http.StatusTooManyRequests, "24 小时内最多提交 10 条，晚点再来吧")
		return
	}
	f, err := a.Store.AddFeedback(r.Context(), s.ID, Feedback{Kind: in.Kind, Content: in.Content, Contact: in.Contact})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.Log.Info("feedback", "sitter", s.ID, "id", f.ID, "kind", f.Kind)
	writeJSON(w, http.StatusOK, map[string]any{"feedback": f})
}

func (a *API) listFeedback(w http.ResponseWriter, r *http.Request, s Sitter) {
	list, err := a.Store.ListFeedback(r.Context(), s.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedback": list})
}
