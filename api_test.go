package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFetcher struct{ got []string }

func (f *fakeFetcher) Fetch(_ context.Context, ids []string) ([][]byte, error) {
	f.got = ids
	out := make([][]byte, len(ids))
	for i := range ids {
		out[i] = []byte{0xff, 0xd8, 0xff, 0xe0} // JPEG magic
	}
	return out, nil
}

type testEnv struct {
	t       *testing.T
	srv     *httptest.Server
	api     *API
	fetcher *fakeFetcher
}

func newEnv(t *testing.T, models ...LLM) *testEnv {
	t.Helper()
	store, err := OpenStore("sqlite", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeFetcher{}
	api := &API{
		Store:  store,
		Model:  &Fallback{Models: models, PerTry: 2 * time.Second},
		Photos: f,
		Travel: 30 * time.Minute,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return &testEnv{t: t, srv: srv, api: api, fetcher: f}
}

func (e *testEnv) do(openid, method, path, body string) (int, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if openid != "" {
		req.Header.Set("X-WX-OPENID", openid)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *testEnv) must(status int, openid, method, path, body string) map[string]any {
	e.t.Helper()
	got, out := e.do(openid, method, path, body)
	if got != status {
		e.t.Fatalf("%s %s: status %d, want %d: %v", method, path, got, status, out)
	}
	return out
}

// onboard creates a consenting sitter with one client and one visit, returning the visit path.
func (e *testEnv) onboard(openid string) string {
	e.must(200, openid, "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	c := e.must(201, openid, "POST", "/api/clients", `{"name":"王姐","pets":"橘猫 大橘","community":"翠苑一区"}`)
	cid := int64(c["client"].(map[string]any)["id"].(float64))
	v := e.must(201, openid, "POST", "/api/visits", `{"client_id":`+itoa(cid)+`,"date":"2027-02-05","time":"09:00","duration_min":40,"days":1}`)
	return "/api/visits/" + itoa(int64(v["ids"].([]any)[0].(float64)))
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestRequiresOpenID(t *testing.T) {
	e := newEnv(t)
	e.must(401, "", "GET", "/api/me", "")
}

func TestWritesNeedConsent(t *testing.T) {
	e := newEnv(t)
	e.must(403, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"猫"}`)
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":false}`)
	e.must(403, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"猫"}`)
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	e.must(201, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"猫"}`)
}

func TestSittersCannotSeeEachOther(t *testing.T) {
	e := newEnv(t)
	visit := e.onboard("o1")
	e.must(200, "o2", "PUT", "/api/me", `{"display_name":"阿强","consent":true}`)
	e.must(404, "o2", "GET", visit, "")
	out := e.must(200, "o2", "GET", "/api/clients", "")
	if n := len(out["clients"].([]any)); n != 0 {
		t.Fatalf("o2 sees %d clients", n)
	}
}

func TestConflictThenForce(t *testing.T) {
	e := newEnv(t)
	e.onboard("o1")
	c := e.must(201, "o1", "POST", "/api/clients", `{"name":"李哥","pets":"柯基"}`)
	cid := itoa(int64(c["client"].(map[string]any)["id"].(float64)))
	// 3 days from 02-03 at 09:20: only 02-05 overlaps the existing 09:00 visit.
	body := `{"client_id":` + cid + `,"date":"2027-02-03","time":"09:20","duration_min":40,"days":3`
	out := e.must(409, "o1", "POST", "/api/visits", body+`}`)
	if n := len(out["conflicts"].([]any)); n != 1 {
		t.Fatalf("got %d conflicts: %v", n, out)
	}
	e.must(201, "o1", "POST", "/api/visits", body+`,"force":true}`)
	list := e.must(200, "o1", "GET", "/api/visits?date=2027-02-03&days=3", "")
	if n := len(list["visits"].([]any)); n != 4 {
		t.Fatalf("got %d visits, want 4", n)
	}
}

func TestVisitLifecycle(t *testing.T) {
	e := newEnv(t)
	visit := e.onboard("o1")
	e.must(409, "o1", "POST", visit+"/leave", "")
	e.must(200, "o1", "POST", visit+"/arrive", "")
	e.must(409, "o1", "POST", visit+"/arrive", "")
	out := e.must(200, "o1", "POST", visit+"/leave", "")
	v := out["visit"].(map[string]any)
	if v["status"] != "done" || v["arrived_at"].(float64) == 0 || v["left_at"].(float64) == 0 {
		t.Fatalf("visit = %v", v)
	}
	e.must(409, "o1", "POST", visit+"/cancel", "")
}

// chatServer fakes an OpenAI-compatible endpoint returning content, or failing with status.
func chatServer(t *testing.T, status int, content string, seen *map[string]any) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("unexpected request %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if passModeration(w, body) {
			return
		}
		if seen != nil {
			json.Unmarshal(body, seen)
		}
		if status != 200 {
			http.Error(w, `{"error":"bad"}`, status)
			return
		}
		b, _ := json.Marshal(content)
		w.Write([]byte(`{"choices":[{"message":{"content":` + string(b) + `}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReportFallsBackAndCanBeEdited(t *testing.T) {
	var seen map[string]any
	broken := chatServer(t, 400, "", nil)
	good := chatServer(t, 200, "```json\n{\"food\":\"猫粮吃完了\",\"water\":\"\",\"litter\":\"已铲\",\"mood\":\"很亲人\",\"anomaly\":\"\",\"note\":\"大橘今天很乖\"}\n```", &seen)
	e := newEnv(t,
		&OpenAICompat{BaseURL: broken.URL, Model: "vision-exp", APIKey: "k", HTTP: http.DefaultClient},
		&OpenAICompat{BaseURL: good.URL, Model: "backup-vl", APIKey: "k", HTTP: http.DefaultClient},
	)
	visit := e.onboard("o1")
	e.must(200, "o1", "POST", visit+"/arrive", "")
	e.must(400, "o1", "POST", visit+"/report", "")
	for i := 0; i < 8; i++ {
		e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/p`+itoa(int64(i))+`.jpg"}`)
	}
	e.must(400, "o1", "POST", visit+"/photos", `{"file_id":"https://evil/x.jpg"}`)

	out := e.must(200, "o1", "POST", visit+"/report", "")
	report := out["report"].(map[string]any)
	fields := report["fields"].(map[string]any)
	if report["model"] != "backup-vl" || fields["food"] != "猫粮吃完了" || fields["water"] != unseen || fields["anomaly"] != "无" {
		t.Fatalf("report = %v", report)
	}
	if len(e.fetcher.got) != maxReportPhotos || !strings.HasSuffix(e.fetcher.got[0], "p2.jpg") {
		t.Fatalf("fetched %v, want the latest %d", e.fetcher.got, maxReportPhotos)
	}
	content := seen["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(content) != 1+maxReportPhotos || !strings.Contains(content[0].(map[string]any)["text"].(string), "橘猫 大橘") {
		t.Fatalf("model request content = %v", content)
	}

	out = e.must(200, "o1", "PUT", visit+"/report", `{"food":"只吃了一半","water":"喝了","litter":"已铲","mood":"好","anomaly":"","note":""}`)
	report = out["report"].(map[string]any)
	if report["model"] != "backup-vl" || report["fields"].(map[string]any)["food"] != "只吃了一半" {
		t.Fatalf("edited report = %v", report)
	}
}

func TestReportWithoutModelLetsSitterWriteManually(t *testing.T) {
	e := newEnv(t)
	visit := e.onboard("o1")
	e.must(200, "o1", "POST", visit+"/arrive", "")
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/a.jpg"}`)
	out := e.must(503, "o1", "POST", visit+"/report", "")
	if out["error"] != ErrNoModel.Error() {
		t.Fatalf("error = %v", out["error"])
	}
	out = e.must(200, "o1", "PUT", visit+"/report", `{"food":"吃完了"}`)
	if f := out["report"].(map[string]any)["fields"].(map[string]any); f["food"] != "吃完了" || f["mood"] != unseen {
		t.Fatalf("fields = %v", f)
	}
	if out["visit"].(map[string]any)["has_report"] != true {
		t.Fatal("has_report not set")
	}
}

type stubModel struct {
	name string
	err  error
}

func (m stubModel) Name() string { return m.name }
func (m stubModel) Vision() bool { return true }
func (m stubModel) Chat(ctx context.Context, req ChatRequest) (string, error) {
	if m.err != nil {
		return "", m.err
	}
	if strings.Contains(req.Prompt, "内容安全审核员") {
		return `{"safe":true}`, nil
	}
	return `{"food":"ok"}`, nil
}

func TestFallbackJoinsErrors(t *testing.T) {
	f := &Fallback{Models: []LLM{stubModel{"a", errors.New("a down")}, stubModel{"b", errors.New("b down")}}, PerTry: time.Second}
	_, _, _, err := GenerateReport(context.Background(), f, ReportInput{})
	if err == nil || !strings.Contains(err.Error(), "a down") || !strings.Contains(err.Error(), "b down") {
		t.Fatalf("err = %v", err)
	}
}

func TestPaidExpires(t *testing.T) {
	now := time.Now()
	if (Sitter{Plan: "season", PlanUntil: now.Add(-time.Hour).Unix()}).Paid(now) {
		t.Fatal("expired plan still paid")
	}
	if !(Sitter{Plan: "season", PlanUntil: now.Add(time.Hour).Unix()}).Paid(now) {
		t.Fatal("active plan not paid")
	}
}
