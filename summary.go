package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SummaryText is the end-of-stay summary a sitter sends the owner as one long image. The model
// drafts it from the stay's reports; the sitter can rewrite every line.
type SummaryText struct {
	Title   string       `json:"title"`
	Overall string       `json:"overall"`
	Days    []SummaryDay `json:"days"`
	Thanks  string       `json:"thanks"`
}

type SummaryDay struct {
	Date string `json:"date"` // YYYY-MM-DD, Shanghai
	Line string `json:"line"`
}

type StoredSummary struct {
	Model     string      `json:"model"`
	Text      SummaryText `json:"text"`
	UpdatedAt int64       `json:"updated_at"`
}

// A stay is the client's run of visits around one visit: neighbours belong together when no
// more than stayGap days apart, or when they were booked together (within bookedTogether of each
// other), which keeps a cat fed every third day or a weekly visit as one stay.
const (
	stayGap        = 2
	bookedTogether = 5 * 60 // seconds; one booking may take a few requests, one per clock time
	stayLookout    = 62 * 24 * time.Hour
)

func sameStay(a, b Visit) bool {
	gap := b.CreatedAt - a.CreatedAt
	if gap < 0 {
		gap = -gap
	}
	return dayGap(ymd(a.StartAt), ymd(b.StartAt)) <= stayGap || gap <= bookedTogether
}

type stayDay struct {
	Date   string `json:"date"`
	Visits int    `json:"visits"`
	Photo  *Photo `json:"photo"`
	ids    []int64
}

type stay struct {
	visits []Visit
	days   []stayDay
}

func ymd(unix int64) string { return time.Unix(unix, 0).In(Shanghai).Format("2006-01-02") }

func dayGap(a, b string) int {
	ta, _ := time.ParseInLocation("2006-01-02", a, Shanghai)
	tb, _ := time.ParseInLocation("2006-01-02", b, Shanghai)
	return int(tb.Sub(ta).Hours()/24 + 0.5)
}

func (a *API) loadStay(ctx context.Context, s Sitter, v Visit) (stay, error) {
	at := time.Unix(v.StartAt, 0)
	all, err := a.Store.ClientVisits(ctx, s.ID, v.ClientID, at.Add(-stayLookout), at.Add(stayLookout))
	if err != nil {
		return stay{}, err
	}
	anchor := -1
	for i, x := range all {
		if x.ID == v.ID {
			anchor = i
		}
	}
	if anchor < 0 { // a cancelled visit is not part of any stay
		return stay{}, ErrNotFound
	}
	lo, hi := anchor, anchor
	for lo > 0 && sameStay(all[lo-1], all[lo]) {
		lo--
	}
	for hi < len(all)-1 && sameStay(all[hi], all[hi+1]) {
		hi++
	}
	st := stay{visits: all[lo : hi+1]}
	for _, x := range st.visits {
		d := ymd(x.StartAt)
		if n := len(st.days); n == 0 || st.days[n-1].Date != d {
			st.days = append(st.days, stayDay{Date: d})
		}
		last := &st.days[len(st.days)-1]
		last.Visits++
		last.ids = append(last.ids, x.ID)
	}
	return st, nil
}

// visited counts the visits the sitter actually made; planned ones are not 上门 yet.
func (st stay) visited() int {
	n := 0
	for _, v := range st.visits {
		if v.Status == "arrived" || v.Status == "done" {
			n++
		}
	}
	return n
}

func (st stay) ids() []int64 {
	out := make([]int64, len(st.visits))
	for i, v := range st.visits {
		out[i] = v.ID
	}
	return out
}

func (a *API) withPhotos(ctx context.Context, st *stay) error {
	photos, err := a.Store.FirstPhotos(ctx, st.ids())
	if err != nil {
		return err
	}
	for i := range st.days {
		for _, id := range st.days[i].ids {
			if p, ok := photos[id]; ok {
				st.days[i].Photo = &p
				break
			}
		}
	}
	return nil
}

func (a *API) summaryContext(w http.ResponseWriter, r *http.Request, s Sitter) (Visit, stay, bool) {
	v, ok := a.loadVisit(w, r, s)
	if !ok {
		return v, stay{}, false
	}
	st, err := a.loadStay(r.Context(), s, v)
	if errors.Is(err, ErrNotFound) {
		writeErr(w, http.StatusConflict, "这一单已取消，没有寄养总结")
		return v, st, false
	}
	if err != nil {
		a.fail(w, err)
		return v, st, false
	}
	if err := a.withPhotos(r.Context(), &st); err != nil {
		a.fail(w, err)
		return v, st, false
	}
	return v, st, true
}

func (a *API) writeSummary(w http.ResponseWriter, s Sitter, v Visit, st stay, sum *StoredSummary, degraded bool, notice string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": sum, "degraded": degraded, "notice": notice,
		"pets": v.Pets, "days": st.days, "visits": st.visited(), "booked": len(st.visits),
		"from": st.visits[0].StartAt, "to": st.visits[len(st.visits)-1].StartAt,
		"sitter_name": s.DisplayName, "paid": s.Paid(time.Now()),
	})
}

// GET /api/visits/{id}/summary: the stay around this visit and its saved summary, if any.
func (a *API) getSummary(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, st, ok := a.summaryContext(w, r, s)
	if !ok {
		return
	}
	sum, err := a.Store.GetSummary(r.Context(), s.ID, st.visits[0].ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	if sum != nil {
		sum.Text = fitDays(sum.Text, st, nil)
	}
	a.writeSummary(w, s, v, st, sum, false, "")
}

// POST /api/visits/{id}/summary drafts the summary with AI, or from the reports alone when AI is
// off, over its daily limit or down.
func (a *API) makeSummary(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, st, ok := a.summaryContext(w, r, s)
	if !ok {
		return
	}
	reports, err := a.Store.ReportsFor(r.Context(), st.ids())
	if err != nil {
		a.fail(w, err)
		return
	}
	if len(reports) == 0 {
		writeErr(w, http.StatusBadRequest, "这次寄养还没有报告，先给上门生成报告")
		return
	}
	text, model, notice := SummaryText{}, "", ""
	switch {
	case !a.aiOn(FeatureSummary):
		notice = "AI 总结已关闭，先按每次报告整理，可以自己改"
	case len(a.Model.Models) == 0:
		notice = "AI 还没配置，先按每次报告整理，可以自己改"
	case !a.allow(FeatureSummary, s.ID):
		notice = "今天 AI 总结次数用完了，先按每次报告整理，可以自己改"
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 55*time.Second)
		text, model, err = GenerateSummary(ctx, a.Model, v.Pets, st, reports)
		cancel()
		if err != nil {
			a.Log.Error("generate summary", "visit", v.ID, "err", err)
			notice = "AI 暂时不可用，先按每次报告整理，可以自己改"
		}
	}
	degraded := model == ""
	text = fitDays(text, st, reports)
	if degraded {
		text = fallbackSummary(v.Pets, st, reports)
	}
	if err := a.Store.SaveSummary(r.Context(), s.ID, st.visits[0].ID, model, text); err != nil {
		a.fail(w, err)
		return
	}
	a.writeSummary(w, s, v, st, &StoredSummary{Model: model, Text: text, UpdatedAt: time.Now().Unix()}, degraded, notice)
}

// PUT /api/visits/{id}/summary saves the sitter's edits.
func (a *API) putSummary(w http.ResponseWriter, r *http.Request, s Sitter) {
	v, st, ok := a.summaryContext(w, r, s)
	if !ok {
		return
	}
	var in SummaryText
	if !readJSON(w, r, &in) {
		return
	}
	if msg := checkSummary(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	old, err := a.Store.GetSummary(r.Context(), s.ID, st.visits[0].ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	model := ""
	if old != nil {
		model = old.Model
	}
	text := fitDays(in, st, nil)
	if err := a.Store.SaveSummary(r.Context(), s.ID, st.visits[0].ID, model, text); err != nil {
		a.fail(w, err)
		return
	}
	a.writeSummary(w, s, v, st, &StoredSummary{Model: model, Text: text, UpdatedAt: time.Now().Unix()}, false, "")
}

func checkSummary(t SummaryText) string {
	if len([]rune(t.Title)) > 30 || len([]rune(t.Overall)) > 200 || len([]rune(t.Thanks)) > 80 || len(t.Days) > 70 {
		return "内容太长了"
	}
	for _, d := range t.Days {
		if len([]rune(d.Line)) > 60 {
			return "每天一句最多 60 字"
		}
	}
	return ""
}

// fitDays keeps exactly one line per stay day, in order: lines for dates outside the stay are
// dropped and missing days get a line from that day's reports (or stay empty without reports).
func fitDays(t SummaryText, st stay, reports map[int64]ReportFields) SummaryText {
	byDate := map[string]string{}
	for _, d := range t.Days {
		byDate[strings.TrimSpace(d.Date)] = strings.TrimSpace(d.Line)
	}
	t.Days = make([]SummaryDay, len(st.days))
	for i, d := range st.days {
		line := byDate[d.Date]
		if line == "" && reports != nil {
			line = dayLine(d, reports)
		}
		t.Days[i] = SummaryDay{Date: d.Date, Line: line}
	}
	t.Title, t.Overall, t.Thanks = strings.TrimSpace(t.Title), strings.TrimSpace(t.Overall), strings.TrimSpace(t.Thanks)
	return t
}

// dayLine is the no-AI line for a day: the sitter's note to the owner, else the pet's mood.
func dayLine(d stayDay, reports map[int64]ReportFields) string {
	for _, id := range d.ids {
		f, ok := reports[id]
		if !ok {
			continue
		}
		if f.Note != "" {
			return f.Note
		}
		if f.Mood != unseen {
			return f.Mood
		}
	}
	for _, id := range d.ids {
		if _, ok := reports[id]; ok {
			return "这天的情况都记在当天的报告里"
		}
	}
	return ""
}

func petName(pets string) string {
	if r := []rune(strings.TrimSpace(pets)); len(r) > 10 {
		return string(r[:10])
	}
	return strings.TrimSpace(pets)
}

// fallbackSummary builds the summary from the reports alone.
func fallbackSummary(pets string, st stay, reports map[int64]ReportFields) SummaryText {
	alerts := 0
	for _, f := range reports {
		if f.Alert != "无" || f.Anomaly != "无" {
			alerts++
		}
	}
	overall := fmt.Sprintf("这次一共上门 %d 次，其中 %d 次有拍照报告。", st.visited(), len(reports))
	if alerts > 0 {
		overall += fmt.Sprintf("其中 %d 次记录了需要留意的情况，详见当天报告。", alerts)
	} else {
		overall += "期间没有发现异常。"
	}
	t := SummaryText{
		Title:   fmt.Sprintf("%s的 %d 天", petName(pets), len(st.days)),
		Overall: overall,
		Thanks:  "谢谢信任，下次出门还可以找我～",
	}
	return fitDays(t, st, reports)
}

const summaryPrompt = `你在帮上门喂宠的宠托师，给宠物主人写一份这次寄养的总结，会做成一张长图发给宠主。
宠物：%s。从 %s 到 %s，共上门 %d 次。
下面是每天的上门报告：
%s
只根据这些记录写，不编造没有记录的事，不做诊断、不说病名。
只输出一个 JSON 对象：
{"title":"不超过 16 字的标题，带上宠物名字，比如「大橘的 7 天寄养日记」","overall":"不超过 100 字的整体小结：吃喝、如厕、精神、遛狗陪玩、有没有需要留意的变化","days":[{"date":"YYYY-MM-DD","line":"这一天不超过 30 字的一句话，像宠物日记，生动但不夸张"}],"thanks":"宠托师对宠主说的一句话，不超过 40 字"}
days 必须覆盖上面每一个日期，date 原样照抄。`

// GenerateSummary drafts the stay summary from its reports. Text only: photos are not resent.
func GenerateSummary(ctx context.Context, f *Fallback, pets string, st stay, reports map[int64]ReportFields) (SummaryText, string, error) {
	var lines []string
	for _, d := range st.days {
		for _, id := range d.ids {
			r, ok := reports[id]
			if !ok {
				continue
			}
			r = r.masked() // sitters may have typed contact details into a report
			lines = append(lines, fmt.Sprintf("- %s：进食 %s；饮水 %s；如厕 %s；精神 %s；异常 %s；留意 %s；给主人的话 %s",
				d.Date, r.Food, r.Water, r.Litter, r.Mood, r.Anomaly, r.Alert, r.Note))
		}
	}
	prompt := fmt.Sprintf(summaryPrompt, maskText(pets), st.days[0].Date, st.days[len(st.days)-1].Date, st.visited(), strings.Join(lines, "\n"))
	var text SummaryText
	model, _, err := f.Do(ctx, ChatRequest{Prompt: prompt, JSON: true, Temperature: 0.5}, func(raw string) error {
		text = SummaryText{} // a rejected reply from the previous model must not leak into this one
		if err := json.Unmarshal([]byte(jsonObject(raw)), &text); err != nil {
			return fmt.Errorf("summary is not JSON: %.200s", raw)
		}
		if strings.TrimSpace(text.Title) == "" || strings.TrimSpace(text.Overall) == "" {
			return fmt.Errorf("summary misses title or overall: %.200s", raw)
		}
		return nil
	})
	if err != nil {
		return SummaryText{}, "", err
	}
	// Clip what the model overwrote; the sitter can still edit within the same limits.
	clip := func(s string, n int) string {
		s = stripDiagnosis(s)
		if r := []rune(s); len(r) > n {
			return string(r[:n])
		}
		return s
	}
	text.Title, text.Overall, text.Thanks = clip(text.Title, 30), clip(text.Overall, 200), clip(text.Thanks, 80)
	for i := range text.Days {
		text.Days[i].Line = clip(text.Days[i].Line, 60)
	}
	return text, model, nil
}
