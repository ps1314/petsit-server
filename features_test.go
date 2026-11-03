package main

import (
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func clientID(out map[string]any) string {
	return itoa(int64(out["client"].(map[string]any)["id"].(float64)))
}

func TestClientContactValidation(t *testing.T) {
	e := newEnv(t)
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	for _, bad := range []string{
		`{"name":"王姐","pets":"猫","phone":"138abc"}`,
		`{"name":"王姐","pets":"猫","phone":"12345"}`,
		`{"name":"王姐","pets":"猫","phone":"123456789012345678901"}`,
		`{"name":"王姐","pets":"猫","wechat":"wang jie"}`,
		`{"name":"王姐","pets":"猫","checklist":["` + strings.Repeat("喂", 41) + `"]}`,
		`{"name":"王姐","pets":"猫","checklist":["1","2","3","4","5","6","7","8","9","10","11","12","13"]}`,
	} {
		e.must(400, "o1", "POST", "/api/clients", bad)
	}
	out := e.must(201, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"猫","phone":" 138 0013-8000 ","wechat":"wangjie_88","checklist":[" 换水 ","","换水","铲猫砂"]}`)
	c := out["client"].(map[string]any)
	if c["phone"] != "13800138000" || c["wechat"] != "wangjie_88" {
		t.Fatalf("client = %v", c)
	}
	if got := c["checklist"].([]any); len(got) != 2 || got[0] != "换水" || got[1] != "铲猫砂" {
		t.Fatalf("checklist = %v", got)
	}
	e.must(201, "o1", "POST", "/api/clients", `{"name":"李哥","pets":"狗","phone":"+8602112345678"}`)
	list := e.must(200, "o1", "GET", "/api/clients", "")["clients"].([]any)
	if list[1].(map[string]any)["phone"] != "13800138000" || len(list[0].(map[string]any)["checklist"].([]any)) != 0 {
		t.Fatalf("clients = %v", list)
	}
	// Twelve items of 40 characters is the most a client can hold.
	items := make([]string, 12)
	for i := range items {
		items[i] = `"` + strings.Repeat("喂", 38) + itoa(int64(10+i)) + `"`
	}
	e.must(200, "o1", "PUT", "/api/clients/"+clientID(out), `{"name":"王姐","pets":"猫","checklist":[`+strings.Join(items, ",")+`]}`)
}

// onboardChecklist is onboard with a client that has a phone, WeChat ID and checklist.
func (e *testEnv) onboardChecklist(openid string) (visit, client string) {
	e.must(200, openid, "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	c := e.must(201, openid, "POST", "/api/clients", `{"name":"王姐","pets":"橘猫 大橘","community":"翠苑一区","phone":"13800138000","wechat":"wangjie_88","checklist":["早上半碗猫粮","换水","铲猫砂","喂药 1 粒"]}`)
	v := e.must(201, openid, "POST", "/api/visits", `{"client_id":`+clientID(c)+`,"date":"2027-02-05","time":"09:00","duration_min":40,"days":1}`)
	return "/api/visits/" + itoa(int64(v["ids"].([]any)[0].(float64))), "/api/clients/" + clientID(c)
}

func ticksOf(out map[string]any) []any { return out["visit"].(map[string]any)["ticks"].([]any) }

func TestChecklistTicks(t *testing.T) {
	e := newEnv(t)
	visit, client := e.onboardChecklist("o1")
	out := e.must(200, "o1", "GET", visit, "")
	v := out["visit"].(map[string]any)
	if v["phone"] != "13800138000" || len(v["checklist"].([]any)) != 4 || len(ticksOf(out)) != 0 {
		t.Fatalf("visit = %v", v)
	}
	e.must(409, "o1", "PUT", visit+"/ticks", `{"ticks":["换水"]}`)
	e.must(200, "o1", "POST", visit+"/arrive", "")
	e.must(400, "o1", "PUT", visit+"/ticks", `{"ticks":["遛狗"]}`)
	out = e.must(200, "o1", "PUT", visit+"/ticks", `{"ticks":["铲猫砂","换水"]}`)
	if got := ticksOf(out); len(got) != 2 || got[0] != "换水" || got[1] != "铲猫砂" {
		t.Fatalf("ticks = %v, want checklist order", got)
	}
	// Saving the same ticks again is not "not found".
	e.must(200, "o1", "PUT", visit+"/ticks", `{"ticks":["换水","铲猫砂"]}`)
	e.must(200, "o2", "PUT", "/api/me", `{"display_name":"阿强","consent":true}`)
	e.must(404, "o2", "PUT", visit+"/ticks", `{"ticks":[]}`)

	// The owner drops 铲猫砂 from the checklist: this visit keeps its record and can still untick it.
	e.must(200, "o1", "PUT", client, `{"name":"王姐","pets":"橘猫 大橘","checklist":["早上半碗猫粮","换水"]}`)
	if got := ticksOf(e.must(200, "o1", "GET", visit, "")); len(got) != 2 {
		t.Fatalf("ticks after checklist edit = %v", got)
	}
	e.must(200, "o1", "POST", visit+"/leave", "")
	if got := ticksOf(e.must(200, "o1", "PUT", visit+"/ticks", `{"ticks":["换水"]}`)); len(got) != 1 || got[0] != "换水" {
		t.Fatalf("ticks after leave = %v", got)
	}
}

func TestReportPromptHasChecklistButNoContact(t *testing.T) {
	var seen map[string]any
	good := chatServer(t, 200, `{"food":"照片里看不出来","water":"宠托师已换水","litter":"照片里看不出来","mood":"照片里看不出来","anomaly":"无","note":"大橘很好"}`, &seen)
	e := newEnv(t, &OpenAICompat{BaseURL: good.URL, Model: "m", APIKey: "k", HTTP: http.DefaultClient})
	visit, _ := e.onboardChecklist("o1")
	e.must(200, "o1", "POST", visit+"/arrive", "")
	e.must(200, "o1", "PUT", visit+"/ticks", `{"ticks":["换水","喂药 1 粒"]}`)
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/a.jpg"}`)
	e.must(200, "o1", "POST", visit+"/report", "")
	prompt := seen["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{"勾选已完成：「换水」「喂药 1 粒」", "没有勾选的事项：「早上半碗猫粮」「铲猫砂」", unseen} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	for _, leak := range []string{"13800138000", "wangjie_88", "王姐"} {
		if strings.Contains(prompt, leak) {
			t.Errorf("prompt leaks %q", leak)
		}
	}
}

func TestPromptWithoutChecklist(t *testing.T) {
	p := buildPrompt(ReportInput{Pets: "猫"})
	if strings.Contains(p, "勾选") {
		t.Fatalf("prompt mentions a checklist that does not exist:\n%s", p)
	}
	p = buildPrompt(ReportInput{Pets: "猫", Checklist: []string{"换水"}})
	if !strings.Contains(p, "勾选已完成：无") || !strings.Contains(p, "没有勾选的事项：「换水」") {
		t.Fatalf("prompt = %s", p)
	}
}

func TestAvailability(t *testing.T) {
	e := newEnv(t)
	e.onboard("o1") // 2027-02-05 09:00
	c := e.must(201, "o1", "POST", "/api/clients", `{"name":"李哥","pets":"柯基"}`)
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+clientID(c)+`,"date":"2027-02-04","time":"18:00","duration_min":40,"days":3}`)
	// A late visit belongs to its Shanghai day, and cancelled ones do not count.
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+clientID(c)+`,"date":"2027-02-06","time":"23:30","duration_min":20,"days":1}`)
	cancelled := e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+clientID(c)+`,"date":"2027-02-04","time":"07:00","duration_min":20,"days":1,"force":true}`)
	e.must(200, "o1", "POST", "/api/visits/"+itoa(int64(cancelled["ids"].([]any)[0].(float64)))+"/cancel", "")

	out := e.must(200, "o1", "GET", "/api/availability?from=2027-02-03&to=2027-02-07", "")
	if out["capacity"].(float64) != 8 || out["sitter_name"] != "小林" {
		t.Fatalf("availability = %v", out)
	}
	days := out["days"].([]any)
	want := []float64{0, 1, 2, 2, 0}
	if len(days) != len(want) {
		t.Fatalf("days = %v", days)
	}
	for i, d := range days {
		if d.(map[string]any)["booked"].(float64) != want[i] {
			t.Fatalf("day %d = %v, want %v booked", i, d, want[i])
		}
	}
	if strings.Contains(e.raw("o1", "/api/availability?from=2027-02-03&to=2027-02-07"), "李哥") {
		t.Fatal("availability exposes client names")
	}

	for _, bad := range []string{`{"capacity":0}`, `{"capacity":31}`, `{"capacity":5,"off_days":["2027/02/06"]}`} {
		e.must(400, "o1", "PUT", "/api/me/availability", bad)
	}
	me := e.must(200, "o1", "PUT", "/api/me/availability", `{"capacity":2,"off_days":["2027-02-07","2027-02-03","2027-02-07"]}`)
	if s := me["sitter"].(map[string]any); s["capacity"].(float64) != 2 || len(s["off_days"].([]any)) != 2 || s["off_days"].([]any)[0] != "2027-02-03" {
		t.Fatalf("sitter = %v", s)
	}
	days = e.must(200, "o1", "GET", "/api/availability?from=2027-02-03&to=2027-02-07", "")["days"].([]any)
	if days[0].(map[string]any)["off"] != true || days[1].(map[string]any)["off"] != false {
		t.Fatalf("days = %v", days)
	}

	e.must(400, "o1", "GET", "/api/availability?from=2027-02-07&to=2027-02-03", "")
	e.must(400, "o1", "GET", "/api/availability?from=2027-01-01&to=2027-03-15", "")
	e.must(200, "o1", "GET", "/api/availability?from=2027-01-01&to=2027-03-03", "") // 62 days
	if n := len(e.must(200, "o2", "GET", "/api/availability?from=2027-02-03&to=2027-02-07", "")["days"].([]any)); n != 5 {
		t.Fatalf("o2 days = %d", n)
	}
}

func (e *testEnv) raw(openid, path string) string {
	e.t.Helper()
	req, _ := http.NewRequest("GET", e.srv.URL+path, nil)
	req.Header.Set("X-WX-OPENID", openid)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// A database created before this batch gains the new columns on startup, and reopening is harmless.
func TestOpenStoreAddsColumnsToOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE sitters (id INTEGER PRIMARY KEY AUTOINCREMENT, openid TEXT NOT NULL UNIQUE, display_name TEXT NOT NULL DEFAULT '', plan TEXT NOT NULL DEFAULT 'free', plan_until INTEGER NOT NULL DEFAULT 0, consent_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE clients (id INTEGER PRIMARY KEY AUTOINCREMENT, sitter_id INTEGER NOT NULL, name TEXT NOT NULL, pets TEXT NOT NULL DEFAULT '', community TEXT NOT NULL DEFAULT '', note TEXT NOT NULL DEFAULT '', deleted INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE visits (id INTEGER PRIMARY KEY AUTOINCREMENT, sitter_id INTEGER NOT NULL, client_id INTEGER NOT NULL, start_at INTEGER NOT NULL, duration_min INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'planned', arrived_at INTEGER NOT NULL DEFAULT 0, left_at INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`INSERT INTO sitters (openid, display_name, created_at) VALUES ('o1', '小林', 1)`,
		`INSERT INTO clients (sitter_id, name, pets, created_at) VALUES (1, '王姐', '猫', 1)`,
		`INSERT INTO visits (sitter_id, client_id, start_at, duration_min, created_at) VALUES (1, 1, 1801000000, 40, 1)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for i := 0; i < 2; i++ {
		st, err := OpenStore("sqlite", path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s, err := st.SitterByOpenID(t.Context(), "o1")
		if err != nil || s.Capacity != 8 || len(s.OffDays) != 0 {
			t.Fatalf("sitter = %+v, %v", s, err)
		}
		v, err := st.GetVisit(t.Context(), 1, 1)
		if err != nil || v.Phone != "" || len(v.Checklist) != 0 || len(v.Ticks) != 0 {
			t.Fatalf("visit = %+v, %v", v, err)
		}
		st.db.Close()
	}
}
