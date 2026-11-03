package main

import (
	"strings"
	"testing"
)

func TestBatchDeleteVisits(t *testing.T) {
	e := newEnv(t)
	e.onboard("o1") // visit 1
	c := e.must(201, "o1", "POST", "/api/clients", `{"name":"李哥","pets":"柯基"}`)
	cid := itoa(int64(c["client"].(map[string]any)["id"].(float64)))
	e.must(201, "o1", "POST", "/api/visits", `{"client_id":`+cid+`,"date":"2027-02-06","time":"14:00","duration_min":40,"days":3}`) // 2,3,4
	e.must(200, "o1", "POST", "/api/visits/1/arrive", "")
	e.must(201, "o1", "POST", "/api/visits/1/photos", `{"file_id":"cloud://env.b/visits/1/a.jpg"}`)
	e.must(200, "o1", "PUT", "/api/visits/1/report", `{"food":"吃完了"}`)

	victim := e.onboard("o2") // visit 5 belongs to another sitter
	out := e.must(200, "o1", "POST", "/api/visits/batch-delete", `{"ids":[1,2,3,`+strings.TrimPrefix(victim, "/api/visits/")+`,999]}`)
	if out["deleted"].(float64) != 3 {
		t.Fatalf("deleted = %v", out["deleted"])
	}
	e.must(404, "o1", "GET", "/api/visits/1", "")
	e.must(200, "o2", "GET", victim, "") // untouched
	list := e.must(200, "o1", "GET", "/api/visits?date=2027-02-05&days=5", "")
	if n := len(list["visits"].([]any)); n != 1 {
		t.Fatalf("%d visits left, want 1", n)
	}
	var photos, reports int
	e.api.Store.db.QueryRow(`SELECT COUNT(*) FROM photos WHERE visit_id = 1`).Scan(&photos)
	e.api.Store.db.QueryRow(`SELECT COUNT(*) FROM reports WHERE visit_id = 1`).Scan(&reports)
	if photos != 0 || reports != 0 {
		t.Fatalf("photos %d reports %d left", photos, reports)
	}
	e.must(400, "o1", "POST", "/api/visits/batch-delete", `{"ids":[]}`)
}
