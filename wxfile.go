package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type PhotoFetcher interface {
	Fetch(ctx context.Context, fileIDs []string) ([][]byte, error)
}

const maxPhotoBytes = 5 << 20

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
		} `json:"file_list"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("batchdownloadfile: %w", err)
	}
	if out.ErrCode != 0 {
		return nil, fmt.Errorf("batchdownloadfile: %d %s", out.ErrCode, out.ErrMsg)
	}
	photos := make([][]byte, 0, len(out.FileList))
	for _, item := range out.FileList {
		if item.Status != 0 || item.DownloadURL == "" {
			continue // expired by the 30-day lifecycle rule or deleted
		}
		b, err := f.download(ctx, item.DownloadURL)
		if err != nil {
			return nil, err
		}
		photos = append(photos, b)
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
	return io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes))
}
