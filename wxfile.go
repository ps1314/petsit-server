package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type PhotoFetcher interface {
	Fetch(ctx context.Context, fileIDs []string) ([][]byte, error)
}

// maxMediaBytes caps one downloaded file: compressed photos are well under it, 60-second videos too.
const maxMediaBytes = 60 << 20

// maxImageBytes caps a photo: the app compresses them, and each one goes to the model whole.
const maxImageBytes = 5 << 20

var ErrTooLarge = errors.New("file too large")

// readCapped reads a whole body, failing rather than silently truncating a file over the cap:
// a cut-off video cannot be decoded and a cut-off photo is not the photo.
func readCapped(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxMediaBytes+1))
	if err == nil && len(b) > maxMediaBytes {
		return nil, ErrTooLarge
	}
	return b, err
}

// WXFetcher downloads photos the mini program put in cloud storage. Inside WeChat Cloud
// Hosting, with the open API service enabled, api.weixin.qq.com needs no access_token.
type WXFetcher struct {
	Env     string
	APIBase string
	HTTP    *http.Client
}

func (f *WXFetcher) Fetch(ctx context.Context, fileIDs []string) ([][]byte, error) {
	list := make([]map[string]any, len(fileIDs))
	for i, id := range fileIDs {
		list[i] = map[string]any{"fileid": id, "max_age": 600}
	}
	body, _ := json.Marshal(map[string]any{"env": f.Env, "file_list": list})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.APIBase+"/tcb/batchdownloadfile", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		ErrCode  int    `json:"errcode"`
		ErrMsg   string `json:"errmsg"`
		FileList []struct {
			FileID      string `json:"fileid"`
			DownloadURL string `json:"download_url"`
			Status      int    `json:"status"`
			ErrMsg      string `json:"errmsg"`
		} `json:"file_list"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("batchdownloadfile: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	if out.ErrCode != 0 {
		return nil, fmt.Errorf("batchdownloadfile: %d %s", out.ErrCode, out.ErrMsg)
	}
	if len(out.FileList) == 0 {
		return nil, fmt.Errorf("batchdownloadfile: empty file_list for %d files: %.300s", len(fileIDs), raw)
	}
	photos := make([][]byte, 0, len(out.FileList))
	var failed []string
	for _, item := range out.FileList {
		if item.Status != 0 || item.DownloadURL == "" {
			// Deleted by the 30-day lifecycle rule, or unreadable (permissions, wrong env).
			failed = append(failed, fmt.Sprintf("%s: status %d %s", item.FileID, item.Status, item.ErrMsg))
			continue
		}
		b, err := f.download(ctx, item.DownloadURL)
		if err != nil {
			return nil, err
		}
		photos = append(photos, b)
	}
	if len(photos) == 0 && len(failed) > 0 {
		return nil, fmt.Errorf("batchdownloadfile: no file readable: %s", strings.Join(failed, "; "))
	}
	return photos, nil
}

func (f *WXFetcher) download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download photo: HTTP %d", resp.StatusCode)
	}
	return readCapped(resp.Body)
}
