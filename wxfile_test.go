package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTCB answers batchdownloadfile like WeChat does: per-file status, plus the file bytes.
func fakeTCB(t *testing.T, status map[string]int) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			w.Write([]byte{0xff, 0xd8})
			return
		}
		var in struct {
			FileList []struct{ FileID string } `json:"file_list"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		var list []map[string]any
		for _, f := range in.FileList {
			item := map[string]any{"fileid": f.FileID, "status": status[f.FileID]}
			if status[f.FileID] == 0 {
				item["download_url"] = srv.URL + "/file"
			} else {
				item["errmsg"] = "STORAGE_EXCEED_AUTHORITY"
			}
			list = append(list, item)
		}
		json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "file_list": list})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchExplainsUnreadablePhotos(t *testing.T) {
	srv := fakeTCB(t, map[string]int{"a": -503003, "b": -503003})
	f := &WXFetcher{Env: "e", APIBase: srv.URL, HTTP: http.DefaultClient}
	_, err := f.Fetch(context.Background(), []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "STORAGE_EXCEED_AUTHORITY") || !strings.Contains(err.Error(), "-503003") {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchKeepsReadablePhotos(t *testing.T) {
	srv := fakeTCB(t, map[string]int{"a": 0, "b": -1})
	f := &WXFetcher{Env: "e", APIBase: srv.URL, HTTP: http.DefaultClient}
	got, err := f.Fetch(context.Background(), []string{"a", "b"})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d photos, err %v", len(got), err)
	}
}
