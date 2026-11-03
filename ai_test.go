package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedModel struct {
	name   string
	text   bool
	calls  atomic.Int32
	err    error
	answer string
	seen   []ChatRequest
}

func (m *scriptedModel) Name() string { return m.name }
func (m *scriptedModel) Vision() bool { return !m.text }
func (m *scriptedModel) Chat(_ context.Context, req ChatRequest) (string, error) {
	m.calls.Add(1)
	m.seen = append(m.seen, req)
	return m.answer, m.err
}

func TestBreakerOpensAndProbes(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b := &Breaker{Failures: 2, Cooldown: time.Minute, now: func() time.Time { return now }}
	allow := func() bool { ok, _ := b.Allow(); return ok }
	if !allow() || b.Failure(false) {
		t.Fatal("first failure must not open")
	}
	if !allow() || !b.Failure(false) || b.State() != BreakerOpen || allow() {
		t.Fatal("second failure must open and block")
	}
	now = now.Add(time.Minute)
	ok, probe := b.Allow()
	if b.State() != BreakerHalfOpen || !ok || !probe || allow() {
		t.Fatal("after cooldown exactly one probe goes out")
	}
	// A cancelled probe hands the slot back without reopening.
	b.Release(true)
	if ok, probe = b.Allow(); !ok || !probe || b.State() != BreakerHalfOpen {
		t.Fatal("released probe not reusable")
	}
	if !b.Failure(true) || b.State() != BreakerOpen {
		t.Fatal("a failed probe reopens at once")
	}
	now = now.Add(time.Minute)
	b.Allow()
	b.Success()
	if b.State() != BreakerClosed || !allow() {
		t.Fatal("a good probe closes")
	}
}

func TestFallbackSkipsTrippedModel(t *testing.T) {
	bad := &scriptedModel{name: "bad", err: errors.New("HTTP 500: boom")}
	good := &scriptedModel{name: "good", answer: `{"food":"吃完了"}`}
	trips := 0
	f := &Fallback{Models: []LLM{bad, good}, PerTry: time.Second, BreakerFailures: 2, BreakerCooldown: time.Hour,
		OnTrip: func(string) { trips++ }}
	for i := 0; i < 5; i++ {
		fields, _, model, err := GenerateReport(context.Background(), f, ReportInput{Photos: [][]byte{{1}}})
		if err != nil || model != "good" || fields.Food != "吃完了" {
			t.Fatalf("round %d: %v %s %v", i, err, model, fields)
		}
	}
	if bad.calls.Load() != 2 || trips != 1 {
		t.Fatalf("bad called %d times, trips %d; want 2 and 1", bad.calls.Load(), trips)
	}
	st := f.Status()
	if st[0]["state"] != BreakerOpen || st[1]["state"] != BreakerClosed {
		t.Fatalf("status = %v", st)
	}
}

func TestBadReplyCountsAsFailure(t *testing.T) {
	junk := &scriptedModel{name: "junk", answer: "我看不出来"}
	good := &scriptedModel{name: "good", answer: `{"food":"ok"}`}
	f := &Fallback{Models: []LLM{junk, good}, PerTry: time.Second}
	if _, _, model, err := GenerateReport(context.Background(), f, ReportInput{}); err != nil || model != "good" {
		t.Fatalf("model %s err %v", model, err)
	}
}

func TestAllTrippedFailsFast(t *testing.T) {
	bad := &scriptedModel{name: "bad", err: errors.New("down")}
	f := &Fallback{Models: []LLM{bad}, PerTry: time.Second, BreakerFailures: 1, BreakerCooldown: time.Hour}
	GenerateReport(context.Background(), f, ReportInput{})
	_, _, _, err := GenerateReport(context.Background(), f, ReportInput{})
	if !errors.Is(err, ErrAIUnavailable) || !strings.Contains(err.Error(), "circuit open") || bad.calls.Load() != 1 {
		t.Fatalf("err %v calls %d", err, bad.calls.Load())
	}
}

func TestTextOnlyModelSkipsPhotos(t *testing.T) {
	text := &scriptedModel{name: "text", text: true, answer: `{"food":"x"}`}
	f := &Fallback{Models: []LLM{text}, PerTry: time.Second}
	if f.HasVision() {
		t.Fatal("text-only model claims vision")
	}
	if _, _, _, err := GenerateReport(context.Background(), f, ReportInput{Photos: [][]byte{{1}}}); !errors.Is(err, ErrNoModel) || text.calls.Load() != 0 {
		t.Fatalf("err %v calls %d", err, text.calls.Load())
	}
	if _, _, err := f.Do(context.Background(), ChatRequest{Prompt: "hi"}, func(string) error { return nil }); err != nil {
		t.Fatalf("text task: %v", err)
	}
}

func TestModelsFromEnv(t *testing.T) {
	t.Setenv("AI_MODELS", "AI_PRIMARY,AI_EXTRA,AI_FALLBACK")
	t.Setenv("AI_PRIMARY_API_KEY", "k1")
	t.Setenv("AI_EXTRA_API_KEY", "k2")
	t.Setenv("AI_EXTRA_BASE_URL", "https://example.com/v1")
	t.Setenv("AI_EXTRA_MODEL", "glm")
	t.Setenv("AI_EXTRA_TEXT_ONLY", "true")
	ms := modelsFromEnv(http.DefaultClient)
	if len(ms) != 2 || ms[0].Name() != "deepseek-flash" || ms[1].Name() != "glm" || ms[1].Vision() {
		t.Fatalf("models = %v", ms)
	}
	t.Setenv("AI_EXTRA_BASE_URL", "")
	if ms := modelsFromEnv(http.DefaultClient); len(ms) != 1 {
		t.Fatalf("slot without base url kept: %v", ms)
	}
}

// reportServer answers report requests with the given alert and records the prompts it saw.
func reportServer(t *testing.T, answers ...string) (*httptest.Server, *[]string) {
	var prompts []string
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		var parts []map[string]any
		var text string
		if json.Unmarshal(body.Messages[0].Content, &parts) == nil {
			text, _ = parts[0]["text"].(string)
		} else {
			json.Unmarshal(body.Messages[0].Content, &text)
		}
		prompts = append(prompts, text)
		a := answers[min(n, len(answers)-1)]
		n++
		b, _ := json.Marshal(a)
		w.Write([]byte(`{"choices":[{"message":{"content":` + string(b) + `}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &prompts
}

func (e *testEnv) stay(openid string, days int) (string, []string) {
	e.must(200, openid, "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	c := e.must(201, openid, "POST", "/api/clients", `{"name":"王姐","pets":"橘猫 大橘","phone":"13800138000"}`)
	cid := int64(c["client"].(map[string]any)["id"].(float64))
	v := e.must(201, openid, "POST", "/api/visits", `{"client_id":`+itoa(cid)+`,"date":"2027-02-05","time":"09:00","duration_min":40,"days":`+itoa(int64(days))+`}`)
	var paths []string
	for _, id := range v["ids"].([]any) {
		paths = append(paths, "/api/visits/"+itoa(int64(id.(float64))))
	}
	return paths[0], paths
}

func (e *testEnv) visitWithPhoto(openid, path string) {
	e.must(200, openid, "POST", path+"/arrive", "")
	id := strings.TrimPrefix(path, "/api/visits/")
	e.must(201, openid, "POST", path+"/photos", `{"file_id":"cloud://env.bucket/visits/`+id+`/a.jpg"}`)
}

func TestReportComparesWithEarlierVisits(t *testing.T) {
	srv, prompts := reportServer(t,
		`{"food":"剩了一半","water":"喝了","litter":"正常","mood":"好","anomaly":"无","note":"乖","alert":"","owner_msg":"不该出现"}`,
		`{"food":"又剩了一半","water":"喝了","litter":"正常","mood":"好","anomaly":"无","note":"乖","alert":"观察到连续两天剩粮一半","owner_msg":"大橘这两天吃得比平时少，我会继续留意"}`)
	e := newEnv(t, &OpenAICompat{BaseURL: srv.URL, Model: "m", APIKey: "k", HTTP: http.DefaultClient})
	_, visits := e.stay("o1", 2)
	e.visitWithPhoto("o1", visits[0])
	first := e.must(200, "o1", "POST", visits[0]+"/report", "")["report"].(map[string]any)["fields"].(map[string]any)
	if first["alert"] != "无" || first["owner_msg"] != "" {
		t.Fatalf("no alert must drop the owner message: %v", first)
	}
	e.visitWithPhoto("o1", visits[1])
	second := e.must(200, "o1", "POST", visits[1]+"/report", "")["report"].(map[string]any)["fields"].(map[string]any)
	if second["alert"] != "观察到连续两天剩粮一半" || second["owner_msg"] == "" {
		t.Fatalf("fields = %v", second)
	}
	if !strings.Contains((*prompts)[0], "还没有之前的上门记录") {
		t.Errorf("first prompt should say there is no history:\n%s", (*prompts)[0])
	}
	p := (*prompts)[1]
	if !strings.Contains(p, "2月5日：进食 剩了一半") || strings.Contains(p, "13800138000") || strings.Contains(p, "王姐") {
		t.Errorf("second prompt:\n%s", p)
	}
	// With the trend feature off the history stays out of the prompt.
	e.api.FeaturesOff = map[string]bool{FeatureTrend: true}
	e.must(200, "o1", "POST", visits[1]+"/report", "")
	if strings.Contains((*prompts)[2], "剩了一半") {
		t.Errorf("history sent with trend off")
	}
}

func TestSummaryWithAIAndFallback(t *testing.T) {
	srv, prompts := reportServer(t,
		`{"food":"吃完","water":"喝了","litter":"正常","mood":"活泼","anomaly":"无","note":"第一天就很亲人"}`,
		`{"food":"吃完","water":"喝了","litter":"正常","mood":"活泼","anomaly":"无","note":"在窗台晒太阳"}`,
		`{"title":"大橘的春节日记","overall":"三天都吃得好","days":[{"date":"2027-02-05","line":"第一天就很亲人"},{"date":"2030-01-01","line":"多余"}],"thanks":"新年快乐"}`)
	e := newEnv(t, &OpenAICompat{BaseURL: srv.URL, Model: "m", APIKey: "k", HTTP: http.DefaultClient})
	_, visits := e.stay("o1", 3)
	e.must(400, "o1", "POST", visits[1]+"/summary", "")
	e.visitWithPhoto("o1", visits[0])
	e.must(200, "o1", "POST", visits[0]+"/report", "")
	e.visitWithPhoto("o1", visits[2])
	e.must(200, "o1", "POST", visits[2]+"/report", "")

	out := e.must(200, "o1", "POST", visits[1]+"/summary", "")
	if out["visits"] != float64(2) || out["booked"] != float64(3) {
		t.Fatalf("visited %v of %v, want 2 of 3: the third visit has not happened", out["visits"], out["booked"])
	}
	sum := out["summary"].(map[string]any)
	text := sum["text"].(map[string]any)
	days := text["days"].([]any)
	if sum["model"] != "m" || out["degraded"] != false || text["title"] != "大橘的春节日记" || len(days) != 3 {
		t.Fatalf("summary = %v", out)
	}
	// A day the model skipped gets its line from that day's report; a day without a report stays empty.
	if days[1].(map[string]any)["line"] != "" || days[2].(map[string]any)["line"] != "在窗台晒太阳" {
		t.Fatalf("days = %v", days)
	}
	if ds := out["days"].([]any); ds[0].(map[string]any)["photo"] == nil || ds[1].(map[string]any)["photo"] != nil {
		t.Fatalf("stay days = %v", ds)
	}
	if p := (*prompts)[2]; strings.Contains(p, "13800138000") || !strings.Contains(p, "在窗台晒太阳") {
		t.Fatalf("summary prompt:\n%s", p)
	}

	// Edits stick and GET returns them from any visit of the stay.
	e.must(200, "o1", "PUT", visits[0]+"/summary", `{"title":"改过的标题","overall":"好","days":[{"date":"2027-02-06","line":"睡了一天"}],"thanks":"谢谢"}`)
	got := e.must(200, "o1", "GET", visits[2]+"/summary", "")["summary"].(map[string]any)
	if gt := got["text"].(map[string]any); gt["title"] != "改过的标题" || gt["days"].([]any)[1].(map[string]any)["line"] != "睡了一天" || got["model"] != "m" {
		t.Fatalf("after edit = %v", got)
	}
	e.must(400, "o1", "PUT", visits[0]+"/summary", `{"title":"`+strings.Repeat("长", 31)+`"}`)

	// Another sitter cannot see it.
	e.must(200, "o2", "PUT", "/api/me", `{"display_name":"别人","consent":true}`)
	e.must(404, "o2", "GET", visits[0]+"/summary", "")

	// Feature off: built from the reports, no model call.
	calls := len(*prompts)
	e.api.FeaturesOff = map[string]bool{FeatureSummary: true}
	out = e.must(200, "o1", "POST", visits[0]+"/summary", "")
	text = out["summary"].(map[string]any)["text"].(map[string]any)
	if out["degraded"] != true || len(*prompts) != calls || !strings.Contains(text["title"].(string), "大橘") ||
		text["days"].([]any)[0].(map[string]any)["line"] != "第一天就很亲人" || out["notice"] == "" {
		t.Fatalf("fallback summary = %v", out)
	}
}

func TestSummaryStaysSplitOnGaps(t *testing.T) {
	e := newEnv(t)
	_, first := e.stay("o1", 2) // 2-05, 2-06
	c := e.must(200, "o1", "GET", "/api/clients", "")["clients"].([]any)[0].(map[string]any)
	cid := int64(c["id"].(float64))
	// 2-08 is two days after 2-06, still the same stay; 2-20 starts another.
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+itoa(cid)+`,"date":"2027-02-08","time":"09:00","duration_min":40,"days":1}`)
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+itoa(cid)+`,"date":"2027-02-20","time":"09:00","duration_min":40,"days":1}`)
	if n := e.must(200, "o1", "GET", first[0]+"/summary", "")["booked"]; n != float64(3) {
		t.Fatalf("stay visits = %v, want 3", n)
	}
	e.must(200, "o1", "POST", first[1]+"/cancel", "")
	e.must(409, "o1", "GET", first[1]+"/summary", "")
}

func TestMaskText(t *testing.T) {
	cases := map[string]string{
		"门锁密码是：2580#":        "2580",
		"密码为2580":            "2580",
		"门禁码 6789":           "6789",
		"取件码 4567":           "4567",
		"开门密码 1 2 3 4":       "1 2 3",
		"电话 138.1234.5678":   "5678",
		"电话 １３８００１３８０００":     "8000",
		"微信号码：lily-2020":     "lily",
		"WeChat: lily_cat99": "lily",
		"加v：lilycat88":       "lilycat",
		"vx号 lilycat88":      "lilycat",
		"住B座1203":            "1203",
		"有事打我13812345678":    "1381",
		"柜子里有 300 克猫粮":       "300",
	}
	for in, leak := range cases {
		if out := maskText(in); strings.Contains(out, leak) {
			t.Errorf("%q -> %q still has %q", in, out, leak)
		}
	}
	if out := maskText("2027年2月5日到2027-02-10"); out != "2027年2月5日到2027-02-10" {
		t.Errorf("dates were masked: %q", out)
	}
}

func TestStripDiagnosis(t *testing.T) {
	f := ReportFields{Alert: "观察到连续两天剩粮。可能是肠胃炎。", OwnerMsg: "大橘疑似感染了，建议去医院"}.undiagnosed()
	if f.Alert != "观察到连续两天剩粮。" || f.OwnerMsg != "" {
		t.Fatalf("fields = %+v", f)
	}
	if f = (ReportFields{Alert: "可能是猫瘟"}).undiagnosed(); f.Alert != "无" {
		t.Fatalf("alert = %q", f.Alert)
	}
}

func TestSitterTextMaskedInHistory(t *testing.T) {
	p := buildPrompt(ReportInput{Pets: "猫", History: []PastReport{{StartAt: 1801789200, Fields: ReportFields{Food: "吃完了", Note: "有事打我13812345678"}}}})
	if strings.Contains(p, "13812345678") {
		t.Fatalf("history leaks a phone number:\n%s", p)
	}
}

func TestMaskIntake(t *testing.T) {
	in := "王姐，2月5号到2月10号帮忙喂猫，电话 138-0013-8000，微信: wangjie_88，翠苑一区 3栋 1-1203，另一套 5栋1202室，门锁密码 902817#"
	masked, phone, wechat := maskIntake(in)
	if phone != "13800138000" || wechat != "wangjie_88" {
		t.Fatalf("phone %q wechat %q", phone, wechat)
	}
	for _, leak := range []string{"138", "wangjie", "1203", "1202", "902817", "3栋"} {
		if strings.Contains(masked, leak) {
			t.Errorf("masked text leaks %q: %s", leak, masked)
		}
	}
	if !strings.Contains(masked, "2月5号到2月10号") || !strings.Contains(masked, "翠苑一区") {
		t.Errorf("masking ate the order details: %s", masked)
	}
}

func TestDraftDropsPlaceholders(t *testing.T) {
	today := time.Date(2026, 11, 3, 0, 0, 0, 0, Shanghai)
	d := IntakeDraft{Note: "门锁密码[密码]，电话[电话]，加v[微信号]，钥匙放地垫下", Community: "翠苑一区[门牌]"}.settle(today)
	if d.Note != "钥匙放地垫下" || d.Community != "翠苑一区" {
		t.Fatalf("note %q community %q", d.Note, d.Community)
	}
}

func TestIntakeRules(t *testing.T) {
	today := time.Date(2026, 11, 3, 0, 0, 0, 0, Shanghai)
	cases := []struct {
		text  string
		start string
		days  int
		times int
		clock []string
		pets  string
	}{
		{"2月5号到2月10号，两只猫，每天两次", "2027-02-05", 6, 2, nil, "两只猫"},
		{"除夕到初六帮忙喂狗，早上8点半", "2027-02-05", 7, 1, []string{"08:30"}, "狗"},
		{"12.20 开始喂一周，下午3点", "2026-12-20", 7, 1, []string{"15:00"}, ""},
		{"11月1日帮喂猫咪", "2027-11-01", 1, 1, nil, "猫咪"},
		{"1月20号开始，一天两次，大概一周", "2027-01-20", 7, 2, nil, ""},
		{"12月1号喂狗，每次1.5小时，早上9.30来", "2026-12-01", 1, 1, []string{"09:30"}, "狗"},
		{"12月2号中午1点喂猫", "2026-12-02", 1, 1, []string{"13:00"}, "猫"},
	}
	for _, c := range cases {
		d := parseIntakeRules(c.text, today).settle(today)
		if d.StartDate != c.start || d.Days != c.days || d.TimesPerDay != c.times || d.Pets != c.pets {
			t.Errorf("%q: got start %s days %d times %d pets %q", c.text, d.StartDate, d.Days, d.TimesPerDay, d.Pets)
		}
		if c.clock != nil && strings.Join(d.Times, ",") != strings.Join(c.clock, ",") {
			t.Errorf("%q: times %v", c.text, d.Times)
		}
	}
}

func intakeEnv(t *testing.T, models ...LLM) *testEnv {
	e := newEnv(t, models...)
	e.api.Clock = func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, Shanghai) }
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	e.must(201, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"橘猫"}`)
	return e
}

func TestIntakeWithAI(t *testing.T) {
	srv, prompts := reportServer(t, `{"client_name":"王姐","pets":"橘猫 大橘","community":"翠苑一区","start_date":"2027-02-05","end_date":"2027-02-10","times_per_day":2,"times":["08:00","25:00"],"duration_min":0,"note":"钥匙放门口地垫下"}`)
	e := intakeEnv(t, &OpenAICompat{BaseURL: srv.URL, Model: "m", APIKey: "k", TextOnly: true, HTTP: http.DefaultClient})
	out := e.must(200, "o1", "POST", "/api/intake", `{"text":"我是王姐，2月5号到10号每天两次，电话13800138000，3栋1203，密码 4321#"}`)
	d := out["draft"].(map[string]any)
	if out["degraded"] != false || d["start_date"] != "2027-02-05" || d["days"] != float64(6) || d["phone"] != "13800138000" ||
		d["matched_client_name"] != "王姐" || d["duration_min"] != float64(40) {
		t.Fatalf("draft = %v", out)
	}
	if ts := d["times"].([]any); len(ts) != 2 || ts[0] != "08:00" || ts[1] != "18:00" {
		t.Fatalf("times = %v", ts)
	}
	p := (*prompts)[0]
	for _, leak := range []string{"13800138000", "1203", "4321"} {
		if strings.Contains(p, leak) {
			t.Errorf("intake prompt leaks %q", leak)
		}
	}
	if !strings.Contains(p, "2026-11-03") || !strings.Contains(p, "2027-02-05") {
		t.Errorf("prompt lacks today or new year eve:\n%s", p)
	}
}

func TestIntakeDegrades(t *testing.T) {
	bad := &scriptedModel{name: "bad", err: errors.New("HTTP 500")}
	e := intakeEnv(t, bad)
	body := `{"text":"2月5号到2月7号喂两只猫"}`
	out := e.must(200, "o1", "POST", "/api/intake", body)
	if out["degraded"] != true || out["draft"].(map[string]any)["days"] != float64(3) || out["notice"] == "" {
		t.Fatalf("degraded draft = %v", out)
	}
	e.api.FeaturesOff = map[string]bool{FeatureIntake: true}
	calls := bad.calls.Load()
	out = e.must(200, "o1", "POST", "/api/intake", body)
	if bad.calls.Load() != calls || out["degraded"] != true {
		t.Fatalf("feature off still called the model")
	}
	e.api.FeaturesOff = nil
	e.api.Limits = map[string]*DailyLimit{FeatureIntake: NewDailyLimit(0)}
	if out = e.must(200, "o1", "POST", "/api/intake", body); out["degraded"] != true || bad.calls.Load() != calls {
		t.Fatalf("over limit still called the model")
	}
	e.must(400, "o1", "POST", "/api/intake", `{"text":"`+strings.Repeat("字", maxIntakeRunes+1)+`"}`)
	e.must(400, "o1", "POST", "/api/intake", `{"text":"  "}`)
}

func TestPricingAndStatus(t *testing.T) {
	e := newEnv(t, &scriptedModel{name: "m"})
	e.must(400, "o1", "PUT", "/api/me/pricing", `{"price":-1}`)
	e.must(400, "o1", "PUT", "/api/me/pricing", `{"price":50,"holiday_extra":10000}`)
	me := e.must(200, "o1", "PUT", "/api/me/pricing", `{"price":60,"holiday_extra":30}`)["sitter"].(map[string]any)
	if me["price"] != float64(60) || me["holiday_extra"] != float64(30) {
		t.Fatalf("sitter = %v", me)
	}
	st := e.must(200, "o1", "GET", "/api/ai/status", "")
	if m := st["models"].([]any); len(m) != 1 || m[0].(map[string]any)["model"] != "m" {
		t.Fatalf("status = %v", st)
	}
}
