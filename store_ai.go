package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (st *Store) UpdatePricing(ctx context.Context, id int64, price, holidayExtra int) error {
	_, err := st.db.ExecContext(ctx, `UPDATE sitters SET price = ?, holiday_extra = ? WHERE id = ?`, price, holidayExtra, id)
	return err
}

// RecentReports returns up to limit reports of the client's visits that started before the given
// time, newest first. Manual reports count too: they are the sitter's own account of the visit.
func (st *Store) RecentReports(ctx context.Context, sitterID, clientID, before int64, limit int) ([]PastReport, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT v.start_at, r.fields FROM visits v JOIN reports r ON r.visit_id = v.id
		WHERE v.sitter_id = ? AND v.client_id = ? AND v.start_at < ? AND v.status NOT IN ('cancelled', 'deleted')
		ORDER BY v.start_at DESC LIMIT ?`, sitterID, clientID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PastReport
	for rows.Next() {
		var p PastReport
		var fields string
		if err := rows.Scan(&p.StartAt, &fields); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(fields), &p.Fields) == nil {
			out = append(out, p)
		}
	}
	return out, rows.Err()
}

// ClientVisits returns the client's live visits starting in [from, to), oldest first.
func (st *Store) ClientVisits(ctx context.Context, sitterID, clientID int64, from, to time.Time) ([]Visit, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+visitCols+` WHERE v.sitter_id = ? AND v.client_id = ? AND v.start_at >= ? AND v.start_at < ?
		AND v.status NOT IN ('cancelled', 'deleted') ORDER BY v.start_at`, sitterID, clientID, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Visit{}
	for rows.Next() {
		v, err := scanVisit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReportsFor loads the reports of the given visits, keyed by visit id.
func (st *Store) ReportsFor(ctx context.Context, visitIDs []int64) (map[int64]ReportFields, error) {
	out := map[int64]ReportFields{}
	if len(visitIDs) == 0 {
		return out, nil
	}
	rows, err := st.db.QueryContext(ctx, `SELECT visit_id, fields FROM reports WHERE visit_id IN (`+placeholders(len(visitIDs))+`)`, int64Args(visitIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var fields string
		if err := rows.Scan(&id, &fields); err != nil {
			return nil, err
		}
		var f ReportFields
		if json.Unmarshal([]byte(fields), &f) == nil {
			out[id] = f.normalized()
		}
	}
	return out, rows.Err()
}

// FirstPhotos returns each visit's first photo, keyed by visit id.
func (st *Store) FirstPhotos(ctx context.Context, visitIDs []int64) (map[int64]Photo, error) {
	out := map[int64]Photo{}
	if len(visitIDs) == 0 {
		return out, nil
	}
	// Only reviewed items with a still image can lead a day on the summary.
	rows, err := st.db.QueryContext(ctx, `SELECT visit_id, id, file_id, kind, thumb_file_id, review, review_reason, received_at FROM photos
		WHERE visit_id IN (`+placeholders(len(visitIDs))+`) AND review = 'ok' AND (kind = 'image' OR thumb_file_id <> '') ORDER BY id`, int64Args(visitIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var vid int64
		var p Photo
		if err := rows.Scan(&vid, &p.ID, &p.FileID, &p.Kind, &p.ThumbFileID, &p.Review, &p.ReviewReason, &p.ReceivedAt); err != nil {
			return nil, err
		}
		if _, ok := out[vid]; !ok {
			out[vid] = p
		}
	}
	return out, rows.Err()
}

func (st *Store) SetReview(ctx context.Context, photoID int64, review, reason string) error {
	_, err := st.db.ExecContext(ctx, `UPDATE photos SET review = ?, review_reason = ? WHERE id = ?`, review, reason, photoID)
	return err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64Args(ids []int64) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// GetSummary returns the stored summary keyed by the first visit of the stay, or nil.
func (st *Store) GetSummary(ctx context.Context, sitterID, firstVisitID int64) (*StoredSummary, error) {
	var s StoredSummary
	var data string
	err := st.db.QueryRowContext(ctx, `SELECT model, data, updated_at FROM summaries WHERE visit_id = ? AND sitter_id = ?`, firstVisitID, sitterID).
		Scan(&s.Model, &data, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, json.Unmarshal([]byte(data), &s.Text)
}

// SaveSummary upserts in one statement: delete-then-insert could deadlock or hit the primary key
// under MySQL's repeatable read when the sitter taps twice.
func (st *Store) SaveSummary(ctx context.Context, sitterID, firstVisitID int64, model string, text SummaryText) error {
	b, _ := json.Marshal(text)
	now := st.now().Unix()
	q := `INSERT INTO summaries (visit_id, sitter_id, model, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE model = VALUES(model), data = VALUES(data), updated_at = VALUES(updated_at)`
	if st.driver == "sqlite" {
		q = `INSERT INTO summaries (visit_id, sitter_id, model, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(visit_id) DO UPDATE SET model = excluded.model, data = excluded.data, updated_at = excluded.updated_at`
	}
	_, err := st.db.ExecContext(ctx, q, firstVisitID, sitterID, model, string(b), now, now)
	return err
}
