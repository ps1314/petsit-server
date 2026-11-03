package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A sitter must not be able to attach another sitter's cloud file: the backend fetches photos
// with server privileges, so the AI would describe someone else's home.
func TestPhotoMustLiveUnderOwnVisit(t *testing.T) {
	e := newEnv(t)
	victim := e.onboard("victim") // visit 1
	e.must(200, "victim", "POST", victim+"/arrive", "")
	e.must(201, "victim", "POST", victim+"/photos", `{"file_id":"cloud://env.bucket/visits/1/home.jpg"}`)

	attacker := e.onboard("attacker") // visit 2
	e.must(200, "attacker", "POST", attacker+"/arrive", "")
	for _, id := range []string{
		"cloud://env.bucket/visits/1/home.jpg",
		"cloud://env.bucket/visits/2/../1/home.jpg",
		"cloud://env.bucket/other/visits/20/x.jpg",
	} {
		e.must(400, "attacker", "POST", attacker+"/photos", `{"file_id":"`+id+`"}`)
	}
	e.must(201, "attacker", "POST", attacker+"/photos", `{"file_id":"cloud://env.bucket/visits/2/mine.jpg"}`)
	// And the victim's visit itself is invisible to the attacker.
	e.must(404, "attacker", "POST", victim+"/photos", `{"file_id":"cloud://env.bucket/visits/1/x.jpg"}`)
}

func TestPhotosNeedArrivalAndAreCapped(t *testing.T) {
	e := newEnv(t)
	visit := e.onboard("o1")
	e.must(409, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/a.jpg"}`)
	e.must(200, "o1", "POST", visit+"/arrive", "")
	for i := 0; i < maxVisitPhotos; i++ {
		e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/p`+itoa(int64(i))+`.jpg"}`)
	}
	e.must(400, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/extra.jpg"}`)
}

func TestReportGenerationIsRateLimited(t *testing.T) {
	e := newEnv(t, stubModel{name: "m"})
	e.api.AILimit = NewDailyLimit(2)
	visit := e.onboard("o1")
	e.must(200, "o1", "POST", visit+"/arrive", "")
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.bucket/visits/1/a.jpg"}`)
	e.must(200, "o1", "POST", visit+"/report", "")
	e.must(200, "o1", "POST", visit+"/report", "")
	e.must(429, "o1", "POST", visit+"/report", "")
	// Manual edits still work once the AI quota is spent.
	e.must(200, "o1", "PUT", visit+"/report", `{"food":"吃完了"}`)
}

func TestDailyLimitResetsNextDay(t *testing.T) {
	now := time.Date(2027, 2, 5, 23, 59, 0, 0, Shanghai)
	l := NewDailyLimit(1)
	l.now = func() time.Time { return now }
	if !l.Allow(1) || l.Allow(1) {
		t.Fatal("first call allowed, second denied expected")
	}
	if !l.Allow(2) {
		t.Fatal("limit is per sitter")
	}
	now = now.Add(2 * time.Minute)
	if !l.Allow(1) {
		t.Fatal("limit should reset at Shanghai midnight")
	}
}

func TestUnknownVisitActionIsChinese404(t *testing.T) {
	e := newEnv(t)
	visit := e.onboard("o1")
	if out := e.must(404, "o1", "POST", visit+"/hack", ""); out["error"] != "找不到这个操作" {
		t.Fatalf("error = %v", out["error"])
	}
}

func TestVisitCountCap(t *testing.T) {
	e := newEnv(t)
	e.onboard("o1") // 1 visit
	n, err := e.api.Store.Count(t.Context(), "visits", 1)
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := e.api.Store.db.Exec(`UPDATE visits SET status = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.api.Store.Count(t.Context(), "visits", 1); n != 0 {
		t.Fatalf("cancelled visits counted: %d", n)
	}
}

type flakyModel struct {
	calls int
	err   error
}

func (m *flakyModel) Name() string { return "flaky" }
func (m *flakyModel) Vision() bool { return true }
func (m *flakyModel) Chat(context.Context, ChatRequest) (string, error) {
	m.calls++
	if m.calls == 1 {
		return "", m.err
	}
	return `{"food":"ok"}`, nil
}

func TestBusyModelIsRetriedOnce(t *testing.T) {
	m := &flakyModel{err: errors.New("deepseek-flash: HTTP 503: Service is too busy")}
	f := &Fallback{Models: []LLM{m}, PerTry: time.Second}
	if _, _, _, err := GenerateReport(context.Background(), f, ReportInput{}); err != nil || m.calls != 2 {
		t.Fatalf("calls %d err %v", m.calls, err)
	}
	other := &flakyModel{err: errors.New("HTTP 400: bad request")}
	f = &Fallback{Models: []LLM{other}, PerTry: time.Second}
	if _, _, _, err := GenerateReport(context.Background(), f, ReportInput{}); err == nil || other.calls != 1 {
		t.Fatalf("non-busy error retried: calls %d", other.calls)
	}
}
