package main

import (
	"strings"
	"testing"
)

func TestClientVisits(t *testing.T) {
	e := newEnv(t)
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	c := e.must(201, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"猫"}`)
	other := e.must(201, "o1", "POST", "/api/clients", `{"name":"李哥","pets":"狗"}`)
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+clientID(c)+`,"date":"2027-02-01","time":"09:00","duration_min":40,"days":7,"every":2}`)
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+clientID(other)+`,"date":"2027-02-01","time":"15:00","duration_min":40,"days":1}`)
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+clientID(c)+`,"date":"2026-12-25","time":"09:00","duration_min":40,"days":1,"force":true}`)

	out := e.must(200, "o1", "GET", "/api/clients/"+clientID(c)+"/visits", "")
	vs := out["visits"].([]any)
	// Every 2 days over 7 days is days 1, 3, 5, 7, plus the earlier December visit; oldest first.
	if len(vs) != 5 || out["client"].(map[string]any)["name"] != "王姐" {
		t.Fatalf("visits = %v", out)
	}
	if first := vs[0].(map[string]any); first["client_name"] != "王姐" || first["start_at"].(float64) > vs[1].(map[string]any)["start_at"].(float64) {
		t.Fatalf("not oldest first: %v", vs)
	}
	// Another sitter cannot read this client's visits.
	e.must(404, "o2", "GET", "/api/clients/"+clientID(c)+"/visits", "")
}

func TestFeedback(t *testing.T) {
	e := newEnv(t)
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	for _, bad := range []string{
		`{"kind":"other","content":"好用"}`,
		`{"kind":"suggest","content":"  "}`,
		`{"kind":"suggest","content":"` + strings.Repeat("好", 501) + `"}`,
		`{"kind":"suggest","content":"好用","contact":"` + strings.Repeat("1", 61) + `"}`,
	} {
		e.must(400, "o1", "POST", "/api/feedback", bad)
	}
	e.must(200, "o1", "POST", "/api/feedback", `{"kind":"suggest","content":" 希望能导出账单 ","contact":"wx_lin"}`)
	e.must(200, "o1", "POST", "/api/feedback", `{"kind":"complaint","content":"报告生成太慢"}`)
	list := e.must(200, "o1", "GET", "/api/feedback", "")["feedback"].([]any)
	if len(list) != 2 || list[1].(map[string]any)["content"] != "希望能导出账单" || list[1].(map[string]any)["contact"] != "wx_lin" {
		t.Fatalf("feedback = %v", list)
	}
	// Each sitter sees only their own.
	if got := e.must(200, "o2", "GET", "/api/feedback", "")["feedback"].([]any); len(got) != 0 {
		t.Fatalf("o2 sees %v", got)
	}
	// A reply written in the database shows up.
	if _, err := e.api.Store.db.Exec(`UPDATE feedback SET reply = '下个版本加上' WHERE content = '希望能导出账单'`); err != nil {
		t.Fatal(err)
	}
	list = e.must(200, "o1", "GET", "/api/feedback", "")["feedback"].([]any)
	if list[1].(map[string]any)["reply"] != "下个版本加上" {
		t.Fatalf("reply = %v", list[1])
	}
	for i := 2; i < maxFeedbackPerDay; i++ {
		e.must(200, "o1", "POST", "/api/feedback", `{"kind":"bug","content":"第`+itoa(int64(i))+`条"}`)
	}
	e.must(429, "o1", "POST", "/api/feedback", `{"kind":"bug","content":"再来一条"}`)
}
