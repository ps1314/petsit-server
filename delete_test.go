package main

import "testing"

func TestDeleteClientClearsContact(t *testing.T) {
	e := newEnv(t)
	e.must(200, "o1", "PUT", "/api/me", `{"display_name":"小林","consent":true}`)
	e.must(201, "o1", "POST", "/api/clients", `{"name":"王姐","pets":"猫","phone":"13800000000","wechat":"wangjie88"}`)
	e.must(200, "o1", "DELETE", "/api/clients/1", "")
	var phone, wechat string
	if err := e.api.Store.db.QueryRow(`SELECT phone, wechat FROM clients WHERE id = 1`).Scan(&phone, &wechat); err != nil {
		t.Fatal(err)
	}
	if phone != "" || wechat != "" {
		t.Fatalf("contact kept after delete: %q %q", phone, wechat)
	}
}
