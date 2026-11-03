package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// COSFetcher reads photos straight from the cloud hosting object storage (Tencent COS) with
// temporary credentials from /_/cos/getauth, the server-side access path WeChat Cloud Hosting
// documents. It does not depend on storage permission rules or on batchdownloadfile.
type COSFetcher struct {
	APIBase  string // http://api.weixin.qq.com inside cloud hosting
	Region   string // ap-shanghai unless the environment was created elsewhere
	Endpoint string // overrides https://<bucket>.cos.<region>.myqcloud.com in tests
	HTTP     *http.Client
	now      func() time.Time
}

type cosAuth struct {
	TmpSecretID  string `json:"TmpSecretId"`
	TmpSecretKey string `json:"TmpSecretKey"`
	Token        string `json:"Token"`
}

// splitFileID turns cloud://<env>.<bucket>/<key> into bucket and key.
func splitFileID(fileID string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(fileID, "cloud://")
	host, key, ok2 := strings.Cut(rest, "/")
	_, bucket, ok3 := strings.Cut(host, ".")
	if !ok || !ok2 || !ok3 || bucket == "" || key == "" {
		return "", "", fmt.Errorf("unexpected file id %q", fileID)
	}
	return bucket, key, nil
}

func (f *COSFetcher) Fetch(ctx context.Context, fileIDs []string) ([][]byte, error) {
	auth, err := f.auth(ctx)
	if err != nil {
		return nil, err
	}
	photos := make([][]byte, 0, len(fileIDs))
	var failed []string
	for _, id := range fileIDs {
		b, err := f.get(ctx, auth, id)
		if err != nil {
			failed = append(failed, err.Error())
			continue
		}
		photos = append(photos, b)
	}
	if len(photos) == 0 && len(failed) > 0 {
		return nil, fmt.Errorf("cos: no file readable: %s", strings.Join(failed, "; "))
	}
	return photos, nil
}

func (f *COSFetcher) auth(ctx context.Context) (cosAuth, error) {
	var a cosAuth
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.APIBase+"/_/cos/getauth", nil)
	if err != nil {
		return a, err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return a, fmt.Errorf("cos getauth: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err := json.Unmarshal(raw, &a); err != nil || a.TmpSecretID == "" {
		return a, fmt.Errorf("cos getauth: HTTP %d: %.200s", resp.StatusCode, raw)
	}
	return a, nil
}

func (f *COSFetcher) get(ctx context.Context, a cosAuth, fileID string) ([]byte, error) {
	bucket, key, err := splitFileID(fileID)
	if err != nil {
		return nil, err
	}
	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", bucket, f.Region)
	base := "https://" + host
	if f.Endpoint != "" {
		base = f.Endpoint
	}
	path := "/" + key
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Host = host
	now := time.Now
	if f.now != nil {
		now = f.now
	}
	req.Header.Set("Authorization", cosSign(a.TmpSecretID, a.TmpSecretKey, http.MethodGet, path, host, now()))
	req.Header.Set("x-cos-security-token", a.Token)
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%s: HTTP %d %.120s", key, resp.StatusCode, body)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes))
}

// cosSign builds a COS v5 Authorization header that signs only the host header.
func cosSign(secretID, secretKey, method, path, host string, now time.Time) string {
	keyTime := fmt.Sprintf("%d;%d", now.Unix()-60, now.Unix()+600)
	mac := func(key, msg string) string {
		h := hmac.New(sha1.New, []byte(key))
		h.Write([]byte(msg))
		return hex.EncodeToString(h.Sum(nil))
	}
	signKey := mac(secretKey, keyTime)
	httpString := strings.ToLower(method) + "\n" + path + "\n\nhost=" + url.QueryEscape(host) + "\n"
	sum := sha1.Sum([]byte(httpString))
	stringToSign := "sha1\n" + keyTime + "\n" + hex.EncodeToString(sum[:]) + "\n"
	return "q-sign-algorithm=sha1&q-ak=" + secretID + "&q-sign-time=" + keyTime + "&q-key-time=" + keyTime +
		"&q-header-list=host&q-url-param-list=&q-signature=" + mac(signKey, stringToSign)
}

// FallbackFetcher tries each fetcher in turn and reports every failure if none works.
type FallbackFetcher []PhotoFetcher

func (fs FallbackFetcher) Fetch(ctx context.Context, fileIDs []string) ([][]byte, error) {
	var errs []error
	for _, f := range fs {
		photos, err := f.Fetch(ctx, fileIDs)
		if err == nil && len(photos) > 0 {
			return photos, nil
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return nil, errors.Join(errs...)
}
