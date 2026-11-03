package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// IntakeDraft is an order read out of a message the pet owner sent the sitter. It is only a
// draft: the sitter checks it on the form before any client or visit is saved.
type IntakeDraft struct {
	ClientName  string   `json:"client_name"`
	Pets        string   `json:"pets"`
	Community   string   `json:"community"`
	Note        string   `json:"note"`
	Phone       string   `json:"phone"`
	Wechat      string   `json:"wechat"`
	StartDate   string   `json:"start_date"`
	Days        int      `json:"days"`
	Every       int      `json:"every"` // a visit every N days; 1 is daily
	TimesPerDay int      `json:"times_per_day"`
	Times       []string `json:"times"`
	DurationMin int      `json:"duration_min"`
	// MatchedClientID is an existing client whose name matches; 0 means a new client.
	MatchedClientID   int64  `json:"matched_client_id"`
	MatchedClientName string `json:"matched_client_name"`
}

// Lunar new year eve by year, kept in step with the mini program's availability page.
var newYearEve = map[int]string{2026: "2026-02-16", 2027: "2027-02-05", 2028: "2028-01-25", 2029: "2029-02-12"}

const maxIntakeRunes = 2000

// maskIntake pulls the phone number and WeChat ID out for the client record, then masks the
// message like any other text bound for a model.
func maskIntake(text string) (masked, phone, wechat string) {
	text = halfWidth(text)
	if m := rePhone.FindString(text); m != "" {
		phone = strings.NewReplacer(" ", "", "-", "", ".", "").Replace(m)
	}
	if m := reWechat.FindStringSubmatch(text); m != nil {
		wechat = m[2]
	}
	return maskText(text), phone, wechat
}

const intakePrompt = `今天是 %s（星期%s）。最近一次除夕是 %s。
下面是宠主发给上门喂宠宠托师的微信消息，电话、微信号、门牌和密码已经打码。
提取订单信息，只输出一个 JSON 对象：
{"client_name":"宠主称呼，没有就空字符串","pets":"宠物，比如「两只猫：大橘、小白」，没有就空字符串","community":"小区或片区名，不含楼栋门牌，没有就空字符串","start_date":"第一次上门日期 YYYY-MM-DD，没写年份就取今天或之后最近的那天；除夕、初几按上面的除夕推算","end_date":"最后一次上门日期 YYYY-MM-DD，只上门一天就和 start_date 相同","every_days":隔几天上门一次（整数：每天上门是 1，隔一天是 2，三天一次是 3，没说就 1）,"times_per_day":每天上门几次（整数，没说就 1）,"times":["宠主指定的上门时间 HH:MM，没说就留空数组"],"duration_min":每次时长分钟（没说就 0）,"note":"其他交代，比如喂药、猫砂、钥匙怎么交接，不超过 60 字，没有就空字符串"}
消息原文：
"""
%s
"""`

var weekdayCN = []string{"日", "一", "二", "三", "四", "五", "六"}

// nextEve is the lunar new year eve on or after today, or the most recent one while the
// festival is still on.
func nextEve(today time.Time) string {
	for y := today.Year() - 1; y <= today.Year()+1; y++ {
		if eve, ok := newYearEve[y]; ok {
			t, _ := time.ParseInLocation("2006-01-02", eve, Shanghai)
			if !t.AddDate(0, 0, 15).Before(today) {
				return eve
			}
		}
	}
	return ""
}

type intakeAI struct {
	ClientName  string   `json:"client_name"`
	Pets        string   `json:"pets"`
	Community   string   `json:"community"`
	StartDate   string   `json:"start_date"`
	EndDate     string   `json:"end_date"`
	EveryDays   int      `json:"every_days"`
	TimesPerDay int      `json:"times_per_day"`
	Times       []string `json:"times"`
	DurationMin int      `json:"duration_min"`
	Note        string   `json:"note"`
}

func parseIntakeAI(ctx context.Context, f *Fallback, masked string, today time.Time) (IntakeDraft, string, error) {
	prompt := fmt.Sprintf(intakePrompt, today.Format("2006-01-02"), weekdayCN[today.Weekday()], nextEve(today), masked)
	var d IntakeDraft
	model, _, err := f.Do(ctx, ChatRequest{Prompt: prompt, JSON: true, Temperature: 0}, func(raw string) error {
		var out intakeAI
		if err := json.Unmarshal([]byte(jsonObject(raw)), &out); err != nil {
			return fmt.Errorf("intake is not JSON: %.200s", raw)
		}
		d = IntakeDraft{ClientName: out.ClientName, Pets: out.Pets, Community: out.Community, Note: out.Note,
			StartDate: out.StartDate, Every: out.EveryDays, TimesPerDay: out.TimesPerDay, Times: out.Times, DurationMin: out.DurationMin}
		start, err1 := time.ParseInLocation("2006-01-02", out.StartDate, Shanghai)
		end, err2 := time.ParseInLocation("2006-01-02", out.EndDate, Shanghai)
		if err1 == nil && err2 == nil && !end.Before(start) {
			d.Days = int(end.Sub(start).Hours()/24+0.5) + 1
		}
		return nil
	})
	return d, model, err
}

var (
	reDate     = regexp.MustCompile(`(?:(\d{4})\s*[年./-]\s*)?(\d{1,2})\s*[月./]\s*(\d{1,2})\s*[日号]?`)
	reLunar    = regexp.MustCompile(`除夕|大年三十|年三十|初(十|[一二三四五六七八九])`)
	reDayCount = regexp.MustCompile(`(\d{1,2}|[一二两三四五六七八九十]+)\s*(天|晚)`)
	reWeek     = regexp.MustCompile(`一周|一个星期|一个礼拜`)
	reTwice    = regexp.MustCompile(`(每天|一天|每日)\s*(两|2|二)\s*次|早晚各?一次|早晚都`)
	// 三天一次 / 每3天 / 每隔两天 (= every 3) / 隔天 (= every 2).
	reEvery     = regexp.MustCompile(`(每隔|隔)\s*([一二两三四五六七八九十\d]+)\s*天|每\s*([一二两三四五六七八九十\d]+)\s*天\s*(?:来|喂|上门)?\s*一次|([一二两三四五六七八九十\d]+)\s*天\s*(?:来|喂|上门)?\s*一次|隔天`)
	reThrice    = regexp.MustCompile(`(每天|一天|每日)\s*(三|3)\s*次`)
	reDotClock  = regexp.MustCompile(`(早上|上午|中午|下午|晚上)(\s*\d{1,2})\.(\d{2})`)
	// 2月5号到10号: the end day without its month.
	reDayOnly   = regexp.MustCompile(`^\s*(?:到|至|-|~|～)\s*(\d{1,2})\s*[日号]`)
	reOnceNext  = regexp.MustCompile(`^\s*(来|喂|上门)?\s*一次`)
	reTimesNext = regexp.MustCompile(`^\s*[一二两三四五六七八九十\d]+\s*次`)
	reUnitNext  = regexp.MustCompile(`^\s*(个?小时|分钟|斤|公斤|克|勺|元|块|点)`)
	reClock     = regexp.MustCompile(`(早上|上午|中午|下午|晚上)?\s*(\d{1,2})\s*(?:[:：](\d{2})|点\s*(半)?)`)
	rePets      = regexp.MustCompile(`([一二两三四五六七八九十\d]+\s*只)?\s*(猫咪|小猫|猫|狗狗|小狗|狗|兔子|兔|仓鼠|鹦鹉|乌龟|龟)`)
	reCommunity = regexp.MustCompile(`[\p{Han}A-Za-z0-9]{2,12}(小区|花园|公寓|家园|新村|广场|大厦|苑|府|里|城)`)
)

var cnDigit = map[rune]int{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}

func cnNumber(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	n := 0
	for _, r := range s {
		switch {
		case r == '十' && n == 0:
			n = 10
		case r == '十':
			n *= 10
		default:
			n += cnDigit[r]
		}
	}
	return n
}

// parseIntakeRules is the no-AI reading of the message: dates, festival days, visit count,
// clock times, pets and community by pattern. It is rougher, and the page says so.
func parseIntakeRules(masked string, today time.Time) IntakeDraft {
	type hit struct {
		pos int
		day time.Time
	}
	// 早上9.30 is a clock time, not September 30th.
	masked = reDotClock.ReplaceAllString(masked, "$1$2:$3")
	var hits []hit
	for _, m := range reDate.FindAllStringSubmatchIndex(masked, -1) {
		// 1.5小时, 2.5斤: a number with a unit is not a date.
		if sep := strings.TrimSpace(masked[m[5]:m[6]]); sep != "月" && reUnitNext.MatchString(masked[m[1]:]) {
			continue
		}
		mon, _ := strconv.Atoi(masked[m[4]:m[5]])
		day, _ := strconv.Atoi(masked[m[6]:m[7]])
		if mon < 1 || mon > 12 || day < 1 || day > 31 {
			continue
		}
		year := today.Year()
		if m[2] >= 0 {
			year, _ = strconv.Atoi(masked[m[2]:m[3]])
		}
		t := time.Date(year, time.Month(mon), day, 0, 0, 0, 0, Shanghai)
		if m[2] < 0 && t.Before(today.AddDate(0, 0, -1)) {
			t = t.AddDate(1, 0, 0)
		}
		hits = append(hits, hit{m[0], t})
		if e := reDayOnly.FindStringSubmatch(masked[m[1]:]); e != nil {
			if end, _ := strconv.Atoi(e[1]); end > day && end <= 31 {
				hits = append(hits, hit{m[1], t.AddDate(0, 0, end-day)})
			}
		}
	}
	if eve := nextEve(today); eve != "" {
		e, _ := time.ParseInLocation("2006-01-02", eve, Shanghai)
		for _, m := range reLunar.FindAllStringSubmatchIndex(masked, -1) {
			t := e
			if m[2] >= 0 {
				t = e.AddDate(0, 0, cnNumber(masked[m[2]:m[3]]))
			}
			hits = append(hits, hit{m[0], t})
		}
	}
	d := IntakeDraft{TimesPerDay: 1}
	if len(hits) > 0 {
		first, last := hits[0], hits[0]
		for _, h := range hits {
			if h.pos < first.pos {
				first = h
			}
			if h.pos > last.pos {
				last = h
			}
		}
		d.StartDate = first.day.Format("2006-01-02")
		if last.day.After(first.day) {
			d.Days = int(last.day.Sub(first.day).Hours()/24+0.5) + 1
		}
	}
	if d.Days == 0 {
		for _, m := range reDayCount.FindAllStringSubmatchIndex(masked, -1) {
			// 一天两次, 每天, 三天一次, 隔两天 are rates, not the length of the stay.
			before := masked[:m[0]]
			if reTimesNext.MatchString(masked[m[1]:]) || reOnceNext.MatchString(masked[m[1]:]) ||
				strings.HasSuffix(before, "每") || strings.HasSuffix(before, "隔") {
				continue
			}
			d.Days = cnNumber(masked[m[2]:m[3]])
			break
		}
		if d.Days == 0 && reWeek.MatchString(masked) {
			d.Days = 7
		}
	}
	if m := reEvery.FindStringSubmatch(masked); m != nil {
		switch {
		case m[2] != "":
			d.Every = cnNumber(m[2]) + 1
		case m[3] != "":
			d.Every = cnNumber(m[3])
		case m[4] != "":
			d.Every = cnNumber(m[4])
		default:
			d.Every = 2
		}
	}
	if reThrice.MatchString(masked) {
		d.TimesPerDay = 3
	} else if reTwice.MatchString(masked) {
		d.TimesPerDay = 2
	}
	for _, m := range reClock.FindAllStringSubmatch(masked, 3) {
		h, _ := strconv.Atoi(m[2])
		min := 0
		if m[3] != "" {
			min, _ = strconv.Atoi(m[3])
		} else if m[4] != "" {
			min = 30
		}
		if (m[1] == "下午" || m[1] == "晚上") && h < 12 || m[1] == "中午" && h <= 3 {
			h += 12
		}
		if h <= 23 && min <= 59 {
			d.Times = append(d.Times, fmt.Sprintf("%02d:%02d", h, min))
		}
	}
	var pets []string
	seen := map[string]bool{}
	for _, m := range rePets.FindAllString(masked, -1) {
		if m = strings.TrimSpace(m); !seen[m] {
			seen[m] = true
			pets = append(pets, m)
		}
	}
	d.Pets = strings.Join(pets, "、")
	d.Community = reCommunity.FindString(masked)
	return d
}

var defaultTimes = map[int][]string{1: {"09:00"}, 2: {"09:00", "18:00"}, 3: {"08:00", "13:00", "19:00"}}

var rePlaceholder = regexp.MustCompile(`[，,、]?\s*(门锁密码|门禁码|取件码|密码|电话|手机号?|微信号?|加v|vx|门牌号?)?\s*[:：是为]?\s*\[(电话|微信号|门牌|密码|数字)\]`)

// settle makes a draft safe to put on the form whatever the model or the patterns produced.
func (d IntakeDraft) settle(today time.Time) IntakeDraft {
	clip := func(s string, n int) string {
		if r := []rune(strings.TrimSpace(s)); len(r) > n {
			return string(r[:n])
		}
		return strings.TrimSpace(s)
	}
	// The model sees masked text and may echo its placeholders; they mean nothing to the sitter.
	unmask := func(s string) string { return strings.Trim(rePlaceholder.ReplaceAllString(s, ""), "，,、；; ") }
	d.ClientName, d.Pets, d.Community, d.Note = clip(unmask(d.ClientName), 30), clip(unmask(d.Pets), 80), clip(unmask(d.Community), 40), clip(unmask(d.Note), 150)
	start, err := time.ParseInLocation("2006-01-02", d.StartDate, Shanghai)
	if err != nil || start.Before(today.AddDate(0, 0, -1)) || start.After(today.AddDate(0, 0, 400)) {
		d.StartDate = ""
	}
	if d.Days < 1 || d.Days > 60 {
		d.Days = 1
	}
	if d.Every < 1 || d.Every > maxEvery {
		d.Every = 1
	}
	if d.TimesPerDay < 1 || d.TimesPerDay > 3 {
		d.TimesPerDay = 1
	}
	var times []string
	for _, t := range d.Times {
		if _, err := time.Parse("15:04", t); err == nil && len(times) < d.TimesPerDay {
			times = append(times, t)
		}
	}
	for _, t := range defaultTimes[d.TimesPerDay][len(times):] {
		times = append(times, t)
	}
	d.Times = times
	if d.DurationMin < 10 || d.DurationMin > 240 {
		d.DurationMin = 40
	}
	return d
}

// matchClient links the draft to an existing client with the same or an overlapping name.
func matchClient(d IntakeDraft, clients []Client) IntakeDraft {
	name := strings.TrimSpace(d.ClientName)
	if name == "" {
		return d
	}
	var loose *Client
	for i, c := range clients {
		if c.Name == name {
			d.MatchedClientID, d.MatchedClientName = c.ID, c.Name
			return d
		}
		if loose == nil && len([]rune(c.Name)) >= 2 && (strings.Contains(name, c.Name) || strings.Contains(c.Name, name)) {
			loose = &clients[i]
		}
	}
	if loose != nil {
		d.MatchedClientID, d.MatchedClientName = loose.ID, loose.Name
	}
	return d
}

// POST /api/intake {"text": "..."}: reads an owner's message into an order draft.
func (a *API) intake(w http.ResponseWriter, r *http.Request, s Sitter) {
	var in struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	text := strings.TrimSpace(in.Text)
	if text == "" || len([]rune(text)) > maxIntakeRunes {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("把宠主的消息粘贴进来，最多 %d 字", maxIntakeRunes))
		return
	}
	now := a.now().In(Shanghai)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, Shanghai)
	masked, phone, wechat := maskIntake(text)

	var d IntakeDraft
	model, notice := "", ""
	switch {
	case !a.aiOn(FeatureIntake):
		notice = "AI 识别已关闭，已按规则识别，请仔细核对"
	case len(a.Model.Models) == 0:
		notice = "AI 还没配置，已按规则识别，请仔细核对"
	case !a.allow(FeatureIntake, s.ID):
		notice = "今天 AI 识别次数用完了，已按规则识别，请仔细核对"
	default:
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		var err error
		d, model, err = parseIntakeAI(ctx, a.Model, masked, today)
		cancel()
		if err != nil {
			a.Log.Error("intake", "err", err)
			notice = "AI 暂时不可用，已按规则识别，请仔细核对"
		}
	}
	if model == "" {
		d = parseIntakeRules(masked, today)
	}
	d.Phone, _ = cleanPhone(phone)
	d.Wechat = wechat
	d = d.settle(today)
	clients, err := a.Store.ListClients(r.Context(), s.ID)
	if err != nil {
		a.fail(w, err)
		return
	}
	d = matchClient(d, clients)
	writeJSON(w, http.StatusOK, map[string]any{"draft": d, "model": model, "degraded": model == "", "notice": notice})
}

// PUT /api/me/pricing {"price": 60, "holiday_extra": 30}: yuan per visit, yuan added per holiday visit.
func (a *API) putPricing(w http.ResponseWriter, r *http.Request, s Sitter) {
	var in struct {
		Price        int `json:"price"`
		HolidayExtra int `json:"holiday_extra"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Price < 0 || in.Price > 9999 || in.HolidayExtra < 0 || in.HolidayExtra > 9999 {
		writeErr(w, http.StatusBadRequest, "单价和节假日加价都填 0 到 9999 元")
		return
	}
	if err := a.Store.UpdatePricing(r.Context(), s.ID, in.Price, in.HolidayExtra); err != nil {
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

// GET /api/ai/status: which models are configured, their breaker state and switched-off features.
func (a *API) aiStatus(w http.ResponseWriter, r *http.Request, s Sitter) {
	off := []string{}
	for k, v := range a.FeaturesOff {
		if v {
			off = append(off, k)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": a.Model.Status(), "features_off": off})
}
