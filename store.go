package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// Times are stored as unix seconds so the same queries run on MySQL (production)
// and SQLite (tests and local dev).

var schemaMySQL = []string{
	`CREATE TABLE IF NOT EXISTS sitters (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		openid VARCHAR(64) NOT NULL UNIQUE,
		display_name VARCHAR(64) NOT NULL DEFAULT '',
		plan VARCHAR(16) NOT NULL DEFAULT 'free',
		plan_until BIGINT NOT NULL DEFAULT 0,
		consent_at BIGINT NOT NULL DEFAULT 0,
		capacity INT NOT NULL DEFAULT 8,
		off_days TEXT,
		price INT NOT NULL DEFAULT 0,
		holiday_pct INT NOT NULL DEFAULT 0,
		holiday_extra INT NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL
	) DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS clients (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		sitter_id BIGINT NOT NULL,
		name VARCHAR(64) NOT NULL,
		pets VARCHAR(255) NOT NULL DEFAULT '',
		community VARCHAR(128) NOT NULL DEFAULT '',
		note VARCHAR(500) NOT NULL DEFAULT '',
		phone VARCHAR(32) NOT NULL DEFAULT '',
		wechat VARCHAR(64) NOT NULL DEFAULT '',
		checklist TEXT,
		deleted TINYINT NOT NULL DEFAULT 0,
		created_at BIGINT NOT NULL,
		INDEX idx_clients_sitter (sitter_id)
	) DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS visits (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		sitter_id BIGINT NOT NULL,
		client_id BIGINT NOT NULL,
		start_at BIGINT NOT NULL,
		duration_min INT NOT NULL,
		status VARCHAR(16) NOT NULL DEFAULT 'planned',
		arrived_at BIGINT NOT NULL DEFAULT 0,
		left_at BIGINT NOT NULL DEFAULT 0,
		ticks TEXT,
		created_at BIGINT NOT NULL,
		INDEX idx_visits_sitter_start (sitter_id, start_at)
	) DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS photos (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		visit_id BIGINT NOT NULL,
		file_id VARCHAR(512) NOT NULL,
		kind VARCHAR(8) NOT NULL DEFAULT 'image',
		thumb_file_id VARCHAR(512) NOT NULL DEFAULT '',
		review VARCHAR(16) NOT NULL DEFAULT 'ok',
		review_reason VARCHAR(128) NOT NULL DEFAULT '',
		received_at BIGINT NOT NULL,
		INDEX idx_photos_visit (visit_id)
	) DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS reports (
		visit_id BIGINT PRIMARY KEY,
		model VARCHAR(64) NOT NULL DEFAULT '',
		raw TEXT,
		fields TEXT,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	) DEFAULT CHARSET=utf8mb4`,
	`CREATE TABLE IF NOT EXISTS summaries (
		visit_id BIGINT PRIMARY KEY,
		sitter_id BIGINT NOT NULL,
		model VARCHAR(64) NOT NULL DEFAULT '',
		data TEXT,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL
	) DEFAULT CHARSET=utf8mb4`,
}

var schemaSQLite = []string{
	`CREATE TABLE IF NOT EXISTS sitters (id INTEGER PRIMARY KEY AUTOINCREMENT, openid TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL DEFAULT '', plan TEXT NOT NULL DEFAULT 'free', plan_until INTEGER NOT NULL DEFAULT 0, consent_at INTEGER NOT NULL DEFAULT 0, capacity INTEGER NOT NULL DEFAULT 8, off_days TEXT, price INTEGER NOT NULL DEFAULT 0, holiday_pct INTEGER NOT NULL DEFAULT 0, holiday_extra INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS clients (id INTEGER PRIMARY KEY AUTOINCREMENT, sitter_id INTEGER NOT NULL, name TEXT NOT NULL, pets TEXT NOT NULL DEFAULT '', community TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', phone TEXT NOT NULL DEFAULT '', wechat TEXT NOT NULL DEFAULT '', checklist TEXT, deleted INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS visits (id INTEGER PRIMARY KEY AUTOINCREMENT, sitter_id INTEGER NOT NULL, client_id INTEGER NOT NULL, start_at INTEGER NOT NULL, duration_min INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'planned', arrived_at INTEGER NOT NULL DEFAULT 0, left_at INTEGER NOT NULL DEFAULT 0, ticks TEXT, created_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS photos (id INTEGER PRIMARY KEY AUTOINCREMENT, visit_id INTEGER NOT NULL, file_id TEXT NOT NULL, kind TEXT NOT NULL DEFAULT 'image', thumb_file_id TEXT NOT NULL DEFAULT '', review TEXT NOT NULL DEFAULT 'ok', review_reason TEXT NOT NULL DEFAULT '', received_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS reports (visit_id INTEGER PRIMARY KEY, model TEXT NOT NULL DEFAULT '', raw TEXT, fields TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS summaries (visit_id INTEGER PRIMARY KEY, sitter_id INTEGER NOT NULL, model TEXT NOT NULL DEFAULT '', data TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
}

// addedColumns brings a database created by an earlier version up to date. Both drivers accept
// this syntax; "duplicate column" errors mean the column is already there.
var addedColumns = []struct{ table, mysql, sqlite string }{
	{"sitters", "capacity INT NOT NULL DEFAULT 8", "capacity INTEGER NOT NULL DEFAULT 8"},
	{"sitters", "off_days TEXT", "off_days TEXT"},
	{"clients", "phone VARCHAR(32) NOT NULL DEFAULT ''", "phone TEXT NOT NULL DEFAULT ''"},
	{"clients", "wechat VARCHAR(64) NOT NULL DEFAULT ''", "wechat TEXT NOT NULL DEFAULT ''"},
	{"clients", "checklist TEXT", "checklist TEXT"},
	{"visits", "ticks TEXT", "ticks TEXT"},
	{"sitters", "price INT NOT NULL DEFAULT 0", "price INTEGER NOT NULL DEFAULT 0"},
	// holiday_pct is unused since the surcharge became a fixed amount; it stays so old rows load.
	{"sitters", "holiday_pct INT NOT NULL DEFAULT 0", "holiday_pct INTEGER NOT NULL DEFAULT 0"},
	{"sitters", "holiday_extra INT NOT NULL DEFAULT 0", "holiday_extra INTEGER NOT NULL DEFAULT 0"},
	{"photos", "kind VARCHAR(8) NOT NULL DEFAULT 'image'", "kind TEXT NOT NULL DEFAULT 'image'"},
	{"photos", "thumb_file_id VARCHAR(512) NOT NULL DEFAULT ''", "thumb_file_id TEXT NOT NULL DEFAULT ''"},
	// Photos taken before review existed count as reviewed.
	{"photos", "review VARCHAR(16) NOT NULL DEFAULT 'ok'", "review TEXT NOT NULL DEFAULT 'ok'"},
	{"photos", "review_reason VARCHAR(128) NOT NULL DEFAULT ''", "review_reason TEXT NOT NULL DEFAULT ''"},
}

var ErrNotFound = errors.New("not found")

type Store struct {
	db     *sql.DB
	driver string
	now    func() time.Time
}

func OpenStore(driver, dsn string) (*Store, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	schema := schemaMySQL
	if driver == "sqlite" {
		schema = schemaSQLite
		db.SetMaxOpenConns(1)
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("create schema: %w", err)
		}
	}
	for _, c := range addedColumns {
		col := c.mysql
		if driver == "sqlite" {
			col = c.sqlite
		}
		if _, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + col); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return nil, fmt.Errorf("add column %s.%s: %w", c.table, col, err)
		}
	}
	return &Store{db: db, driver: driver, now: time.Now}, nil
}

type Sitter struct {
	ID          int64  `json:"id"`
	OpenID      string `json:"-"`
	DisplayName string `json:"display_name"`
	Plan        string `json:"plan"`
	PlanUntil   int64  `json:"plan_until"`
	ConsentAt   int64  `json:"consent_at"`
	// Capacity is how many visits a day the sitter takes; OffDays (YYYY-MM-DD) are days off.
	// Both only feed the availability image.
	Capacity int      `json:"capacity"`
	OffDays  []string `json:"off_days"`
	// Price is yuan per visit and HolidayExtra the yuan added per holiday visit; a 0 price leaves the quote out of the
	// reply the intake page drafts.
	Price      int `json:"price"`
	HolidayExtra int `json:"holiday_extra"`
}

// Paid reports drop the tool watermark until the plan expires.
func (s Sitter) Paid(now time.Time) bool {
	return s.Plan != "free" && s.PlanUntil > now.Unix()
}

func (st *Store) SitterByOpenID(ctx context.Context, openid string) (Sitter, error) {
	var s Sitter
	var offDays string
	q := `SELECT id, openid, display_name, plan, plan_until, consent_at, capacity, COALESCE(off_days, ''), price, holiday_extra FROM sitters WHERE openid = ?`
	scan := func() error {
		return st.db.QueryRowContext(ctx, q, openid).Scan(&s.ID, &s.OpenID, &s.DisplayName, &s.Plan, &s.PlanUntil, &s.ConsentAt, &s.Capacity, &offDays, &s.Price, &s.HolidayExtra)
	}
	err := scan()
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO sitters (openid, created_at) VALUES (?, ?)`, openid, st.now().Unix()); err != nil && !isDuplicate(err) {
			return s, err
		}
		err = scan()
	}
	s.OffDays = decodeList(offDays)
	return s, err
}

// Lists (checklists, ticks, days off) are stored as JSON text so both drivers handle them alike.
func encodeList(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(items)
	return string(b)
}

func decodeList(s string) []string {
	out := []string{}
	if s != "" {
		json.Unmarshal([]byte(s), &out)
	}
	return out
}

func (st *Store) UpdateAvailability(ctx context.Context, id int64, capacity int, offDays []string) error {
	_, err := st.db.ExecContext(ctx, `UPDATE sitters SET capacity = ?, off_days = ? WHERE id = ?`, capacity, encodeList(offDays), id)
	return err
}

func isDuplicate(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "Duplicate entry") || strings.Contains(msg, "UNIQUE constraint")
}

func (st *Store) UpdateSitter(ctx context.Context, id int64, displayName string, consent bool) error {
	if consent {
		_, err := st.db.ExecContext(ctx, `UPDATE sitters SET display_name = ?, consent_at = CASE WHEN consent_at = 0 THEN ? ELSE consent_at END WHERE id = ?`, displayName, st.now().Unix(), id)
		return err
	}
	_, err := st.db.ExecContext(ctx, `UPDATE sitters SET display_name = ? WHERE id = ?`, displayName, id)
	return err
}

type Client struct {
	ID        int64    `json:"id"`
	Name      string   `json:"name"`
	Pets      string   `json:"pets"`
	Community string   `json:"community"`
	Note      string   `json:"note"`
	Phone     string   `json:"phone"`
	Wechat    string   `json:"wechat"`
	Checklist []string `json:"checklist"`
}

const clientCols = `id, name, pets, community, note, phone, wechat, COALESCE(checklist, '')`

func scanClient(row interface{ Scan(...any) error }) (Client, error) {
	var c Client
	var checklist string
	err := row.Scan(&c.ID, &c.Name, &c.Pets, &c.Community, &c.Note, &c.Phone, &c.Wechat, &checklist)
	c.Checklist = decodeList(checklist)
	return c, err
}

func (st *Store) ListClients(ctx context.Context, sitterID int64) ([]Client, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+clientCols+` FROM clients WHERE sitter_id = ? AND deleted = 0 ORDER BY id DESC`, sitterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Client{}
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (st *Store) GetClient(ctx context.Context, sitterID, id int64) (Client, error) {
	c, err := scanClient(st.db.QueryRowContext(ctx, `SELECT `+clientCols+` FROM clients WHERE id = ? AND sitter_id = ? AND deleted = 0`, id, sitterID))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// Count returns a sitter's live rows in clients or visits (table names are fixed, never user input).
func (st *Store) Count(ctx context.Context, table string, sitterID int64) (int, error) {
	q := map[string]string{
		"clients": `SELECT COUNT(*) FROM clients WHERE sitter_id = ? AND deleted = 0`,
		"visits":  `SELECT COUNT(*) FROM visits WHERE sitter_id = ? AND status NOT IN ('cancelled', 'deleted')`,
	}[table]
	var n int
	err := st.db.QueryRowContext(ctx, q, sitterID).Scan(&n)
	return n, err
}

func (st *Store) CreateClient(ctx context.Context, sitterID int64, c Client) (Client, error) {
	res, err := st.db.ExecContext(ctx, `INSERT INTO clients (sitter_id, name, pets, community, note, phone, wechat, checklist, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sitterID, c.Name, c.Pets, c.Community, c.Note, c.Phone, c.Wechat, encodeList(c.Checklist), st.now().Unix())
	if err != nil {
		return c, err
	}
	c.ID, err = res.LastInsertId()
	return c, err
}

func (st *Store) UpdateClient(ctx context.Context, sitterID int64, c Client) error {
	res, err := st.db.ExecContext(ctx, `UPDATE clients SET name = ?, pets = ?, community = ?, note = ?, phone = ?, wechat = ?, checklist = ? WHERE id = ? AND sitter_id = ? AND deleted = 0`,
		c.Name, c.Pets, c.Community, c.Note, c.Phone, c.Wechat, encodeList(c.Checklist), c.ID, sitterID)
	return affected(res, err)
}

func (st *Store) DeleteClient(ctx context.Context, sitterID, id int64) error {
	// Name and pets stay so past visits still read correctly; the owner's contact details go.
	res, err := st.db.ExecContext(ctx, `UPDATE clients SET deleted = 1, phone = '', wechat = '' WHERE id = ? AND sitter_id = ?`, id, sitterID)
	return affected(res, err)
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type Visit struct {
	ID          int64  `json:"id"`
	ClientID    int64  `json:"client_id"`
	ClientName  string `json:"client_name"`
	Pets        string `json:"pets"`
	Community   string `json:"community"`
	StartAt     int64  `json:"start_at"`
	DurationMin int    `json:"duration_min"`
	Status      string `json:"status"`
	ArrivedAt   int64  `json:"arrived_at"`
	LeftAt      int64  `json:"left_at"`
	HasReport   bool   `json:"has_report"`
	// From the client, for calling or messaging the owner and for the per-visit checklist.
	Note      string   `json:"note"`
	Phone     string   `json:"phone"`
	Wechat    string   `json:"wechat"`
	Checklist []string `json:"checklist"`
	// Ticks are the checklist items the sitter marked done on this visit, in checklist order.
	Ticks []string `json:"ticks"`
	// CreatedAt ties together the visits booked in one go, e.g. every third day of a stay.
	CreatedAt int64 `json:"-"`
}

const visitCols = `v.id, v.client_id, c.name, c.pets, c.community, v.start_at, v.duration_min, v.status, v.arrived_at, v.left_at,
	CASE WHEN r.visit_id IS NULL THEN 0 ELSE 1 END, c.note, c.phone, c.wechat, COALESCE(c.checklist, ''), COALESCE(v.ticks, ''), v.created_at
	FROM visits v JOIN clients c ON c.id = v.client_id LEFT JOIN reports r ON r.visit_id = v.id`

func scanVisit(row interface{ Scan(...any) error }) (Visit, error) {
	var v Visit
	var hasReport int
	var checklist, ticks string
	err := row.Scan(&v.ID, &v.ClientID, &v.ClientName, &v.Pets, &v.Community, &v.StartAt, &v.DurationMin, &v.Status, &v.ArrivedAt, &v.LeftAt, &hasReport,
		&v.Note, &v.Phone, &v.Wechat, &checklist, &ticks, &v.CreatedAt)
	v.HasReport = hasReport == 1
	v.Checklist, v.Ticks = decodeList(checklist), decodeList(ticks)
	return v, err
}

// ListVisits returns non-cancelled visits starting in [from, to).
func (st *Store) ListVisits(ctx context.Context, sitterID int64, from, to time.Time) ([]Visit, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT `+visitCols+` WHERE v.sitter_id = ? AND v.start_at >= ? AND v.start_at < ? AND v.status NOT IN ('cancelled', 'deleted') ORDER BY v.start_at`,
		sitterID, from.Unix(), to.Unix())
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

func (st *Store) GetVisit(ctx context.Context, sitterID, id int64) (Visit, error) {
	v, err := scanVisit(st.db.QueryRowContext(ctx, `SELECT `+visitCols+` WHERE v.id = ? AND v.sitter_id = ? AND v.status <> 'deleted'`, id, sitterID))
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	return v, err
}

func (st *Store) CreateVisits(ctx context.Context, sitterID, clientID int64, slots []Slot) ([]int64, error) {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := make([]int64, 0, len(slots))
	for _, s := range slots {
		res, err := tx.ExecContext(ctx, `INSERT INTO visits (sitter_id, client_id, start_at, duration_min, created_at) VALUES (?, ?, ?, ?, ?)`,
			sitterID, clientID, s.Start.Unix(), int(s.Duration/time.Minute), st.now().Unix())
		if err != nil {
			return nil, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, tx.Commit()
}

// DeleteVisits hides the sitter's visits among ids and drops their photo records and reports
// (the photo files themselves expire with the 30-day storage rule). Returns how many it deleted.
func (st *Store) DeleteVisits(ctx context.Context, sitterID int64, ids []int64) (int, error) {
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE visits SET status = 'deleted' WHERE id = ? AND sitter_id = ? AND status <> 'deleted'`, id, sitterID)
		if err != nil {
			return 0, err
		}
		if k, _ := res.RowsAffected(); k == 0 {
			continue
		}
		n++
		for _, q := range []string{`DELETE FROM photos WHERE visit_id = ?`, `DELETE FROM reports WHERE visit_id = ?`, `DELETE FROM summaries WHERE visit_id = ?`} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return 0, err
			}
		}
	}
	return n, tx.Commit()
}

// SetVisitStatus moves a visit along planned -> arrived -> done; any non-done visit can be cancelled.
func (st *Store) SetVisitStatus(ctx context.Context, sitterID, id int64, action string) error {
	now := st.now().Unix()
	var res sql.Result
	var err error
	switch action {
	case "arrive":
		res, err = st.db.ExecContext(ctx, `UPDATE visits SET status = 'arrived', arrived_at = ? WHERE id = ? AND sitter_id = ? AND status = 'planned'`, now, id, sitterID)
	case "leave":
		res, err = st.db.ExecContext(ctx, `UPDATE visits SET status = 'done', left_at = ? WHERE id = ? AND sitter_id = ? AND status = 'arrived'`, now, id, sitterID)
	case "cancel":
		res, err = st.db.ExecContext(ctx, `UPDATE visits SET status = 'cancelled' WHERE id = ? AND sitter_id = ? AND status <> 'done'`, id, sitterID)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return affected(res, err)
}

// SetTicks records which checklist items were done; only while the sitter is or has been there.
func (st *Store) SetTicks(ctx context.Context, sitterID, id int64, ticks []string) error {
	res, err := st.db.ExecContext(ctx, `UPDATE visits SET ticks = ? WHERE id = ? AND sitter_id = ? AND status IN ('arrived', 'done')`, encodeList(ticks), id, sitterID)
	return affected(res, err)
}

// Photo is one photo or video of a visit. Videos carry a cover image (ThumbFileID) that stands in
// for them in reports and long images.
type Photo struct {
	ID          int64  `json:"id"`
	FileID      string `json:"file_id"`
	Kind        string `json:"kind"` // image or video
	ThumbFileID string `json:"thumb_file_id"`
	// Review is the content check: ok, pending (no model could check it yet) or blocked.
	Review       string `json:"review"`
	ReviewReason string `json:"review_reason"`
	ReceivedAt   int64  `json:"received_at"`
	sitterID     int64  // for the review quota; not stored
}

// Shown reports whether the item may appear in what goes to the owner.
func (p Photo) Shown() bool { return p.Review == ReviewOK }

// Still is the image that stands for the item: the photo itself or the video's cover.
func (p Photo) Still() string {
	if p.Kind == KindVideo {
		return p.ThumbFileID
	}
	return p.FileID
}

const (
	KindImage     = "image"
	KindVideo     = "video"
	ReviewOK      = "ok"
	ReviewPending = "pending"
	ReviewBlocked = "blocked"
)

func (st *Store) AddPhoto(ctx context.Context, visitID int64, p Photo) (Photo, error) {
	p.ReceivedAt = st.now().Unix()
	res, err := st.db.ExecContext(ctx, `INSERT INTO photos (visit_id, file_id, kind, thumb_file_id, review, review_reason, received_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		visitID, p.FileID, p.Kind, p.ThumbFileID, p.Review, p.ReviewReason, p.ReceivedAt)
	if err != nil {
		return p, err
	}
	p.ID, err = res.LastInsertId()
	return p, err
}

func (st *Store) ListPhotos(ctx context.Context, visitID int64) ([]Photo, error) {
	rows, err := st.db.QueryContext(ctx, `SELECT id, file_id, kind, thumb_file_id, review, review_reason, received_at FROM photos WHERE visit_id = ? ORDER BY id`, visitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Photo{}
	for rows.Next() {
		var p Photo
		if err := rows.Scan(&p.ID, &p.FileID, &p.Kind, &p.ThumbFileID, &p.Review, &p.ReviewReason, &p.ReceivedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (st *Store) DeletePhoto(ctx context.Context, visitID, photoID int64) error {
	res, err := st.db.ExecContext(ctx, `DELETE FROM photos WHERE id = ? AND visit_id = ?`, photoID, visitID)
	return affected(res, err)
}

type Report struct {
	Model     string       `json:"model"`
	Fields    ReportFields `json:"fields"`
	UpdatedAt int64        `json:"updated_at"`
}

func (st *Store) GetReport(ctx context.Context, visitID int64) (*Report, error) {
	var r Report
	var fields string
	err := st.db.QueryRowContext(ctx, `SELECT model, fields, updated_at FROM reports WHERE visit_id = ?`, visitID).Scan(&r.Model, &fields, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, json.Unmarshal([]byte(fields), &r.Fields)
}

// SaveReport writes the model output (raw is kept for later quality checks) or, with raw empty, the sitter's edits.
func (st *Store) SaveReport(ctx context.Context, visitID int64, model, raw string, f ReportFields) error {
	b, _ := json.Marshal(f)
	now := st.now().Unix()
	if raw == "" {
		res, err := st.db.ExecContext(ctx, `UPDATE reports SET fields = ?, updated_at = ? WHERE visit_id = ?`, string(b), now, visitID)
		if err := affected(res, err); !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM reports WHERE visit_id = ?`, visitID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO reports (visit_id, model, raw, fields, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		visitID, model, raw, string(b), now, now); err != nil {
		return err
	}
	return tx.Commit()
}
