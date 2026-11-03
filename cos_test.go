package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSplitFileID(t *testing.T) {
	b, k, err := splitFileID("cloud://prod-d6gu.7072-prod-d6gu-1524411271/visits/4/1-0.jpg")
	if err != nil || b != "7072-prod-d6gu-1524411271" || k != "visits/4/1-0.jpg" {
		t.Fatalf("%q %q %v", b, k, err)
	}
	for _, bad := range []string{"https://x/y", "cloud://nodot/visits/1.jpg", "cloud://a.b"} {
		if _, _, err := splitFileID(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

// Known-answer check computed independently with Python's hmac/hashlib for the same inputs.
func TestCOSSignKnownAnswer(t *testing.T) {
	got := cosSign("AKID", "SECRET", "GET", "/visits/4/a.jpg", "b-1.cos.ap-shanghai.myqcloud.com", time.Unix(1700000060, 0))
	want := "q-sign-algorithm=sha1&q-ak=AKID&q-sign-time=1700000000;1700000660&q-key-time=1700000000;1700000660&q-header-list=host&q-url-param-list=&q-signature=" + cosSignWant
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestCOSFetcherUsesTemporaryKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_/cos/getauth":
			w.Write([]byte(`{"TmpSecretId":"AKID","TmpSecretKey":"SECRET","Token":"tok","ExpiredTime":1}`))
		case r.URL.Path == "/visits/4/a.jpg":
			if r.Host != "b-1.cos.ap-shanghai.myqcloud.com" || r.Header.Get("x-cos-security-token") != "tok" ||
				!strings.Contains(r.Header.Get("Authorization"), "q-ak=AKID") {
				http.Error(w, "bad auth", http.StatusForbidden)
				return
			}
			w.Write([]byte{0xff, 0xd8})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f := &COSFetcher{APIBase: srv.URL, Region: "ap-shanghai", Endpoint: srv.URL, HTTP: http.DefaultClient}
	got, err := f.Fetch(context.Background(), []string{"cloud://env.b-1/visits/4/a.jpg", "cloud://env.b-1/visits/4/missing.jpg"})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %d, err %v", len(got), err)
	}
}

type stubFetcher struct {
	photos [][]byte
	err    error
}

func (s stubFetcher) Fetch(context.Context, []string) ([][]byte, error) { return s.photos, s.err }

func TestFallbackFetcher(t *testing.T) {
	ok := stubFetcher{photos: [][]byte{{1}}}
	if got, err := (FallbackFetcher{stubFetcher{err: errors.New("tcb down")}, ok}).Fetch(context.Background(), nil); err != nil || len(got) != 1 {
		t.Fatalf("fallback not used: %v", err)
	}
	if got, err := (FallbackFetcher{stubFetcher{}, ok}).Fetch(context.Background(), nil); err != nil || len(got) != 1 {
		t.Fatalf("empty result should fall through: %v", err)
	}
	_, err := (FallbackFetcher{stubFetcher{err: errors.New("a")}, stubFetcher{err: errors.New("b")}}).Fetch(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Fatalf("err = %v", err)
	}
}
