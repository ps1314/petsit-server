package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type API struct {
	Store     *Store
	Model     *Fallback
	Photos    PhotoFetcher
	Travel    time.Duration
	DevOpenID string // local development only; production identity comes from WeChat Cloud Hosting
	Log       *slog.Logger
	AILimit   *DailyLimit // caps report generations per sitter per day; nil means unlimited
}

const (
	maxReportPhotos = 6
	maxVisitPhotos  = 12
	// Caps per sitter that no real sitter reaches but that stop one account from filling the database.
	maxClients = 500
	maxVisits  = 5000
)

func (a *API) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/me", a.auth(a.getMe, false))
	mux.HandleFunc("PUT /api/me", a.auth(a.putMe, false))
	mux.HandleFunc("PUT /api/me/availability", a.auth(a.putAvailability, false))
	mux.HandleFunc("GET /api/availability", a.auth(a.getAvailability, false))
	mux.HandleFunc("GET /api/clients", a.auth(a.listClients, false))
	mux.HandleFunc("POST /api/clients", a.auth(a.createClient, true))
	mux.HandleFunc("PUT /api/clients/{id}", a.auth(a.updateClient, true))
	mux.HandleFunc("DELETE /api/clients/{id}", a.auth(a.deleteClient, true))
	mux.HandleFunc("GET /api/visits", a.auth(a.listVisits, false))
	mux.HandleFunc("POST /api/visits", a.auth(a.createVisits, true))
	mux.HandleFunc("GET /api/visits/{id}", a.auth(a.getVisit, false))
	mux.HandleFunc("POST /api/visits/{id}/{action}", a.auth(a.visitAction, true))
	mux.HandleFunc("DELETE /api/visits/{id}/photos/{photo}", a.auth(a.deletePhoto, true))
	mux.HandleFunc("PUT /api/visits/{id}/report", a.auth(a.putReport, true))
	mux.HandleFunc("PUT /api/visits/{id}/ticks", a.auth(a.putTicks, true))
	return mux
}

type handler func(w http.ResponseWriter, r *http.Request, s Sitter)

// auth resolves the sitter from the openid WeChat Cloud Hosting injects. Public network
// access must stay off for the service, otherwise this header could be forged.
// Writes require the sitter to have confirmed they tell pet owners about photos.
func (a *API) auth(h handler, needConsent bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		openid := r.Header.Get("X-WX-OPENID")
		if openid == "" {
			openid = a.DevOpenID
		}
		if openid == "" {
			writeErr(w, http.StatusUnauthorized, "请在微信小程序里打开")
			return
		}
		s, err := a.Store.SitterByOpenID(r.Context(), openid)
		if err != nil {
			a.fail(w, err)
			return
		}
		if needConsent && s.ConsentAt == 0 {
			writeErr(w, http.StatusForbidden, "请先在「我的」里确认已告知宠主会拍照记录")
			return
		}
		h(w, r, s)
	}
}

func (a *API) getMe(w http.ResponseWriter, r *http.Request, s Sitter) {
	writeJSON(w, http.StatusOK, map[string]any{"sitter": s, "paid": s.Paid(time.Now())})
}

func (a *API) putMe(w http.ResponseWriter, r *http.Request, s Sitter) {
	var in struct {
		DisplayName string `json:"display_name"`
		Consent     bool   `json:"consent"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.DisplayName)
	if name == "" || len([]rune(name)) > 20 {
		writeErr(w, http.StatusBadRequest, "称呼要填，最多 20 个字")
		return
	}
	if err := a.Store.UpdateSitter(r.Context(), s.ID, name, in.Consent); err != nil {
		a.fail(w, err)
		return
	}
	s, err := a.Store.SitterByOpenID(r.Context(), s.OpenID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.getMe(w, r, s)
}

func (a *API) listClients(w http.ResponseWriter, r *http.Request, s Sitter) {
	cs, err := a.Store.ListClients(r.Context(), s.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"clients": cs})
}

func readClient(w http.ResponseWriter, r *http.Request) (Client, bool) {
	var c Client
	if !readJSON(w, r, &c) {
		return c, false
	}
	c.Name, c.Pets, c.Community, c.Note = strings.TrimSpace(c.Name), strings.TrimSpace(c.Pets), strings.TrimSpace(c.Community), strings.TrimSpace(c.Note)
	if c.Name == "" || c.Pets == "" {
		writeErr(w, http.StatusBadRequest, "客户称呼和宠物都要填")
		return c, false
	}
	if len([]rune(c.Name)) > 30 || len([]rune(c.Pets)) > 80 || len([]rune(c.Community)) > 40 || len([]rune(c.Note)) > 150 {
		writeErr(w, http.StatusBadRequest, "内容太长了")
		return c, false
	}
	var msg string
	if c.Phone, msg = cleanPhone(c.Phone); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return c, false
	}
	if c.Wechat, msg = cleanWechat(c.Wechat); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return c, false
	}
	if c.Checklist, msg = cleanChecklist(c.Checklist); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return c, false
	}
	return c, true
}

const (
	maxChecklistItems = 12
	maxChecklistRunes = 40
)

// cleanPhone accepts mobiles, landlines with area codes and +86 numbers; spaces and dashes are dropped.
func cleanPhone(s string) (string, string) {
	s = strings.NewReplacer(" ", "", "-", "", "\u00a0", "").Replace(strings.TrimSpace(s))
	if s == "" {
		return "", ""
	}
	digits := strings.TrimPrefix(s, "+")
	if len(digits) < 7 || len(digits) > 20 || strings.Trim(digits, "0123456789") != "" {
		return s, "电话只填数字，7 到 20 位"
	}
	return s, ""
}

func cleanWechat(s string) (string, string) {
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 40 || strings.ContainsAny(s, " \t\n") {
		return s, "微信号不能有空格，最多 40 个字"
	}
	return s, ""
}

// cleanChecklist trims items and drops blanks and repeats, keeping the sitter's order.
func cleanChecklist(items []string) ([]string, string) {
	out := []string{}
	seen := map[string]bool{}
	for _, it := range items {
		it = strings.TrimSpace(it)
		if it == "" || seen[it] {
			continue
		}
		if len([]rune(it)) > maxChecklistRunes {
			return nil, fmt.Sprintf("清单每项最多 %d 个字", maxChecklistRunes)
		}
		seen[it] = true
		out = append(out, it)
	}
	if len(out) > maxChecklistItems {
		return nil, fmt.Sprintf("清单最多 %d 项", maxChecklistItems)
	}
	return out, ""
}

func (a *API) createClient(w http.ResponseWriter, r *http.Request, s Sitter) {
	c, ok := readClient(w, r)
	if !ok {
		return
	}
	if n, err := a.Store.Count(r.Context(), "clients", s.ID); err != nil {
		a.fail(w, err)
		return
	} else if n >= maxClients {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("客户最多 %d 个", maxClients))
		return
	}
	c, err := a.Store.CreateClient(r.Context(), s.ID, c)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"client": c})
}

func (a *API) updateClient(w http.ResponseWriter, r *http.Request, s Sitter) {
	c, ok := readClient(w, r)
	if !ok {
		return
	}
	if c.ID, ok = pathID(w, r, "id"); !ok {
		return
	}
	if err := a.Store.UpdateClient(r.Context(), s.ID, c); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"client": c})
}

func (a *API) deleteClient(w http.ResponseWriter, r *http.Request, s Sitter) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	if err := a.Store.DeleteClient(r.Context(), s.ID, id); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// GET /api/visits?date=2026-11-07&days=7
func (a *API) listVisits(w http.ResponseWriter, r *http.Request, s Sitter) {
	from, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), Shanghai)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "日期格式不对")
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 62 {
		days = 1
	}
	vs, err := a.Store.ListVisits(r.Context(), s.ID, from, from.AddDate(0, 0, days))
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"visits": vs})
}

func (a *API) createVisits(w http.ResponseWriter, r *http.Request, s Sitter) {
	var in struct {
		ClientID    int64  `json:"client_id"`
		Date        string `json:"date"`
		Time        string `json:"time"`
		DurationMin int    `json:"duration_min"`
		Days        int    `json:"days"`
		Force       bool   `json:"force"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if _, err := a.Store.GetClient(r.Context(), s.ID, in.ClientID); err != nil {
		a.fail(w, err)
		return
	}
	slots, err := BuildSlots(in.Date, in.Time, in.DurationMin, in.Days)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !in.Force {
		first, last := slots[0], slots[len(slots)-1]
		existing, err := a.Store.ListVisits(r.Context(), s.ID, first.Start.Add(-24*time.Hour), last.Start.Add(last.Duration+24*time.Hour))
		if err != nil {
			a.fail(w, err)
			return
		}
		if cs := FindConflicts(slots, existing, a.Travel); len(cs) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{"conflicts": cs, "travel_min": int(a.Travel / time.Minute)})
			return
		}
	}
	if n, err := a.Store.Count(r.Context(), "visits", s.ID); err != nil {
		a.fail(w, err)
		return
	} else if n+len(slots) > maxVisits {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("订单最多 %d 条", maxVisits))
		return
	}
	ids, err := a.Store.CreateVisits(r.Context(), s.ID, in.ClientID, slots)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ids": ids})
}

func (a *API) loadVisit(w http.ResponseWriter, r *http.Request, s Sitter) (Visit, bool) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return Visit{}, false
	}
	v, err := a.Store.GetVisit(r.Context(), s.ID, id)
	if err != nil {
		a.fail(w, err)
		return v, false
	}
	return v, true
}

func (a *API) getVisit(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, ok := a.loadVisit(w, r, s)
	if !ok {
		return
	}
	a.writeVisit(w, r, s, v)
}

func (a *API) writeVisit(w http.ResponseWriter, r *http.Request, s Sitter, v Visit) {
	photos, err := a.Store.ListPhotos(r.Context(), v.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	report, err := a.Store.GetReport(r.Context(), v.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"visit": v, "photos": photos, "report": report,
		"sitter_name": s.DisplayName, "paid": s.Paid(time.Now()),
	})
}

// POST /api/visits/{id}/{arrive|leave|cancel|photos|report}
func (a *API) visitAction(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, ok := a.loadVisit(w, r, s)
	if !ok {
		return
	}
	switch action := r.PathValue("action"); action {
	case "arrive", "leave", "cancel":
		if err := a.Store.SetVisitStatus(r.Context(), s.ID, v.ID, action); err != nil {
			if errors.Is(err, ErrNotFound) {
				writeErr(w, http.StatusConflict, "这一单当前状态不能这样操作，刷新看看")
				return
			}
			a.fail(w, err)
			return
		}
	case "photos":
		a.addPhoto(w, r, v)
		return
	case "report":
		if !a.generateReport(w, r, s, v) {
			return
		}
	default:
		writeErr(w, http.StatusNotFound, "找不到这个操作")
		return
	}
	v, err := a.Store.GetVisit(r.Context(), s.ID, v.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeVisit(w, r, s, v)
}

func (a *API) addPhoto(w http.ResponseWriter, r *http.Request, v Visit) {
	var in struct {
		FileID string `json:"file_id"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	// The backend later reads this file with server privileges, bypassing storage permissions,
	// so only accept files under this visit's own folder: a sitter must not be able to attach
	// (and have the AI describe) a photo another sitter uploaded.
	if !strings.HasPrefix(in.FileID, "cloud://") || len(in.FileID) > 500 ||
		!strings.Contains(in.FileID, "/visits/"+strconv.FormatInt(v.ID, 10)+"/") || strings.Contains(in.FileID, "..") {
		writeErr(w, http.StatusBadRequest, "照片上传失败，请重拍")
		return
	}
	if v.Status != "arrived" && v.Status != "done" {
		writeErr(w, http.StatusConflict, "开始上门后才能拍照")
		return
	}
	existing, err := a.Store.ListPhotos(r.Context(), v.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if len(existing) >= maxVisitPhotos {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("每单最多 %d 张照片", maxVisitPhotos))
		return
	}
	p, err := a.Store.AddPhoto(r.Context(), v.ID, in.FileID)
	if err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"photo": p})
}

func (a *API) deletePhoto(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, ok := a.loadVisit(w, r, s)
	if !ok {
		return
	}
	pid, ok := pathID(w, r, "photo")
	if !ok {
		return
	}
	if err := a.Store.DeletePhoto(r.Context(), v.ID, pid); err != nil {
		a.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (a *API) generateReport(w http.ResponseWriter, r *http.Request, s Sitter, v Visit) bool {
	photos, err := a.Store.ListPhotos(r.Context(), v.ID)
	if err != nil {
		a.fail(w, err)
		return false
	}
	if len(photos) == 0 {
		writeErr(w, http.StatusBadRequest, "先拍几张照片再生成报告")
		return false
	}
	if len(photos) > maxReportPhotos {
		photos = photos[len(photos)-maxReportPhotos:]
	}
	if len(a.Model.Models) == 0 {
		writeErr(w, http.StatusServiceUnavailable, ErrNoModel.Error())
		return false
	}
	if a.AILimit != nil && !a.AILimit.Allow(s.ID) {
		writeErr(w, http.StatusTooManyRequests, "今天 AI 生成次数用完了，可以先手动填写报告")
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
	defer cancel()
	ids := make([]string, len(photos))
	for i, p := range photos {
		ids[i] = p.FileID
	}
	images, err := a.Photos.Fetch(ctx, ids)
	if err != nil {
		a.Log.Error("fetch photos", "visit", v.ID, "files", ids, "err", err)
		// The reason is shown during the trial so storage setup problems can be fixed from a screenshot.
		msg := err.Error()
		if len(msg) > 160 {
			msg = msg[:160]
		}
		writeErr(w, http.StatusBadGateway, "照片读取失败（"+msg+"）")
		return false
	}
	if len(images) == 0 {
		writeErr(w, http.StatusBadRequest, "照片都已过期（30 天自动删除），请重新拍")
		return false
	}
	// The checklist grounds the report in what the sitter did. Phone and WeChat ID never go to the model.
	in := ReportInput{Pets: v.Pets, Photos: images, Checklist: v.Checklist, Done: v.Ticks}
	if v.ArrivedAt > 0 {
		in.ArrivedAt = time.Unix(v.ArrivedAt, 0)
	}
	if v.LeftAt > 0 {
		in.LeftAt = time.Unix(v.LeftAt, 0)
	}
	fields, raw, model, err := a.Model.Generate(ctx, in)
	if err != nil {
		a.Log.Error("generate report", "visit", v.ID, "err", err)
		writeErr(w, http.StatusBadGateway, "AI 暂时不可用，可以先手动填写报告")
		return false
	}
	if err := a.Store.SaveReport(r.Context(), v.ID, model, raw, fields); err != nil {
		a.fail(w, err)
		return false
	}
	return true
}

func (a *API) putReport(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, ok := a.loadVisit(w, r, s)
	if !ok {
		return
	}
	var f ReportFields
	if !readJSON(w, r, &f) {
		return
	}
	for _, field := range []string{f.Food, f.Water, f.Litter, f.Mood, f.Anomaly, f.Note} {
		if len([]rune(field)) > 120 {
			writeErr(w, http.StatusBadRequest, "每项最多 120 字")
			return
		}
	}
	if err := a.Store.SaveReport(r.Context(), v.ID, "", "", f.normalized()); err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.Store.GetVisit(r.Context(), s.ID, v.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeVisit(w, r, s, v)
}

// PUT /api/visits/{id}/ticks {"ticks": ["换水"]}: items must come from the client's checklist.
func (a *API) putTicks(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, ok := a.loadVisit(w, r, s)
	if !ok {
		return
	}
	var in struct {
		Ticks []string `json:"ticks"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if v.Status != "arrived" && v.Status != "done" {
		writeErr(w, http.StatusConflict, "开始上门后才能勾清单")
		return
	}
	want := map[string]bool{}
	for _, t := range in.Ticks {
		want[strings.TrimSpace(t)] = true
	}
	// Ticks for items since removed from the checklist stay, so an old report keeps its record.
	ticks := []string{}
	for _, item := range append(append([]string{}, v.Checklist...), v.Ticks...) {
		if want[item] {
			ticks = append(ticks, item)
			delete(want, item)
		}
	}
	if len(want) > 0 {
		writeErr(w, http.StatusBadRequest, "清单已经改过了，刷新看看")
		return
	}
	if err := a.Store.SetTicks(r.Context(), s.ID, v.ID, ticks); err != nil {
		a.fail(w, err)
		return
	}
	v, err := a.Store.GetVisit(r.Context(), s.ID, v.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.writeVisit(w, r, s, v)
}

const (
	defaultCapacity = 8
	maxCapacity     = 30
	maxRangeDays    = 62
)

// PUT /api/me/availability {"capacity": 8, "off_days": ["2027-02-06"]}
func (a *API) putAvailability(w http.ResponseWriter, r *http.Request, s Sitter) {
	var in struct {
		Capacity int      `json:"capacity"`
		OffDays  []string `json:"off_days"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Capacity < 1 || in.Capacity > maxCapacity {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("每天接单数要在 1 到 %d 之间", maxCapacity))
		return
	}
	if len(in.OffDays) > 366 {
		writeErr(w, http.StatusBadRequest, "休息日太多了")
		return
	}
	off := []string{}
	seen := map[string]bool{}
	for _, d := range in.OffDays {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			writeErr(w, http.StatusBadRequest, "日期格式不对")
			return
		}
		if !seen[d] {
			seen[d] = true
			off = append(off, d)
		}
	}
	sort.Strings(off)
	if err := a.Store.UpdateAvailability(r.Context(), s.ID, in.Capacity, off); err != nil {
		a.fail(w, err)
		return
	}
	s, err := a.Store.SitterByOpenID(r.Context(), s.OpenID)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.getMe(w, r, s)
}

type DayLoad struct {
	Date   string `json:"date"`
	Booked int    `json:"booked"`
	Off    bool   `json:"off"`
}

// GET /api/availability?from=2027-01-28&to=2027-02-20 (inclusive). Only counts leave the
// server: the image is posted publicly, so no client names or communities.
func (a *API) getAvailability(w http.ResponseWriter, r *http.Request, s Sitter) {
	q := r.URL.Query()
	from, err1 := time.ParseInLocation("2006-01-02", q.Get("from"), Shanghai)
	to, err2 := time.ParseInLocation("2006-01-02", q.Get("to"), Shanghai)
	if err1 != nil || err2 != nil {
		writeErr(w, http.StatusBadRequest, "日期格式不对")
		return
	}
	if to.Before(from) || to.Sub(from) >= maxRangeDays*24*time.Hour {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("结束日期要在开始日期之后，最多 %d 天", maxRangeDays))
		return
	}
	end := to.AddDate(0, 0, 1)
	vs, err := a.Store.ListVisits(r.Context(), s.ID, from, end)
	if err != nil {
		a.fail(w, err)
		return
	}
	booked := map[string]int{}
	for _, v := range vs {
		booked[time.Unix(v.StartAt, 0).In(Shanghai).Format("2006-01-02")]++
	}
	off := map[string]bool{}
	for _, d := range s.OffDays {
		off[d] = true
	}
	days := []DayLoad{}
	for d := from; d.Before(end); d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		days = append(days, DayLoad{Date: k, Booked: booked[k], Off: off[k]})
	}
	capacity := s.Capacity
	if capacity <= 0 {
		capacity = defaultCapacity
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days": days, "capacity": capacity, "sitter_name": s.DisplayName, "paid": s.Paid(time.Now()),
	})
}

func (a *API) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusNotFound, "找不到这条记录")
		return
	}
	a.Log.Error("request failed", "err", err)
	writeErr(w, http.StatusInternalServerError, "服务出错了，稍后再试")
}

func pathID(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusNotFound, "找不到这条记录")
		return 0, false
	}
	return id, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
