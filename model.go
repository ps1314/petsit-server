package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ReportFields is what a sitter sends to the pet owner. Every field is free text so the
// sitter can rewrite whatever the model got wrong.
type ReportFields struct {
	Food    string `json:"food"`
	Water   string `json:"water"`
	Litter  string `json:"litter"`
	Mood    string `json:"mood"`
	Anomaly string `json:"anomaly"`
	Note    string `json:"note"`
}

const unseen = "照片里看不出来"

func (f ReportFields) normalized() ReportFields {
	fill := func(s string) string {
		if s = strings.TrimSpace(s); s == "" {
			return unseen
		}
		return s
	}
	f.Food, f.Water, f.Litter, f.Mood = fill(f.Food), fill(f.Water), fill(f.Litter), fill(f.Mood)
	f.Anomaly = strings.TrimSpace(f.Anomaly)
	if f.Anomaly == "" {
		f.Anomaly = "无"
	}
	f.Note = strings.TrimSpace(f.Note)
	return f
}

type ReportInput struct {
	Pets      string
	ArrivedAt time.Time
	LeftAt    time.Time
	Photos    [][]byte
	// Checklist is what the owner asked for on every visit; Done is what the sitter ticked this time.
	Checklist []string
	Done      []string
}

type ReportModel interface {
	Name() string
	Generate(ctx context.Context, in ReportInput) (ReportFields, string, error)
}

var ErrNoModel = errors.New("AI 还没配置，可以先手动填写报告")

const reportPrompt = `你在帮上门喂宠的宠托师给宠物主人写当次上门报告。
只根据照片里能看到的内容写，看不清或照片里没有的项目，必须原样写"照片里看不出来"，不许猜，不许写套话。
宠物：%s
到达时间：%s，离开时间：%s
%s
只输出一个 JSON 对象，字段都是简短中文（每项不超过 40 字）：
{"food":"进食情况，比如食盆剩余多少","water":"饮水情况","litter":"猫砂或便便情况","mood":"精神状态","anomaly":"异常情况，没有就写无","note":"给主人的一句话，语气温和，不超过 30 字"}`

// checklistPrompt tells the model which chores the sitter confirmed. Ticked items are the
// sitter's own word and may be stated as done; nothing else may, and what the pet ate or how it
// behaves still has to come from the photos.
const checklistPrompt = `
宠主交代的每次上门事项，宠托师本次勾选已完成：%s
没有勾选的事项：%s
勾选的事项是宠托师确认做过的，可以在对应字段里写成"宠托师已……"；没勾选的不许写成做过。宠物吃了多少、喝了多少、精神怎样仍然只看照片，照片里看不出来就写"照片里看不出来"。
`

func buildPrompt(in ReportInput) string {
	clock := func(t time.Time) string {
		if t.IsZero() {
			return "未记录"
		}
		return t.In(Shanghai).Format("15:04")
	}
	extra := ""
	if len(in.Checklist) > 0 || len(in.Done) > 0 {
		done := map[string]bool{}
		for _, d := range in.Done {
			done[d] = true
		}
		var left []string
		for _, c := range in.Checklist {
			if !done[c] {
				left = append(left, c)
			}
		}
		list := func(items []string) string {
			if len(items) == 0 {
				return "无"
			}
			return "「" + strings.Join(items, "」「") + "」"
		}
		extra = fmt.Sprintf(checklistPrompt, list(in.Done), list(left))
	}
	return fmt.Sprintf(reportPrompt, in.Pets, clock(in.ArrivedAt), clock(in.LeftAt), extra)
}

// OpenAICompat talks to any chat completions endpoint that accepts image_url parts
// (DeepSeek, DashScope compatible mode, Doubao). Swapping models is a config change.
type OpenAICompat struct {
	BaseURL string
	Model   string
	APIKey  string
	HTTP    *http.Client
}

func (m *OpenAICompat) Name() string { return m.Model }

func (m *OpenAICompat) Generate(ctx context.Context, in ReportInput) (ReportFields, string, error) {
	parts := []map[string]any{{"type": "text", "text": buildPrompt(in)}}
	for _, p := range in.Photos {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]string{"url": "data:" + http.DetectContentType(p) + ";base64," + base64.StdEncoding.EncodeToString(p)},
		})
	}
	body, _ := json.Marshal(map[string]any{
		"model":           m.Model,
		"messages":        []map[string]any{{"role": "user", "content": parts}},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0.2,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ReportFields{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return ReportFields{}, "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return ReportFields{}, "", fmt.Errorf("%s: HTTP %d: %.300s", m.Model, resp.StatusCode, respBody)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil || len(out.Choices) == 0 {
		return ReportFields{}, "", fmt.Errorf("%s: unexpected response: %.300s", m.Model, respBody)
	}
	raw := out.Choices[0].Message.Content
	f, err := parseReport(raw)
	if err != nil {
		return ReportFields{}, raw, fmt.Errorf("%s: %w", m.Model, err)
	}
	return f, raw, nil
}

// parseReport tolerates a ```json fence around the object, which some models add even in JSON mode.
func parseReport(raw string) (ReportFields, error) {
	s := strings.TrimSpace(raw)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	var f ReportFields
	if err := json.Unmarshal([]byte(s), &f); err != nil {
		return f, fmt.Errorf("report is not JSON: %.200s", raw)
	}
	return f.normalized(), nil
}

// Fallback tries each model in order, giving each its own deadline, so an outage of the
// experimental DeepSeek vision model still yields a report within the sitter's wait.
type Fallback struct {
	Models    []ReportModel
	PerTry    time.Duration
	RetryWait time.Duration // pause before the one retry of a busy (429/503) model
	OnError   func(model string, err error)
}

func (f *Fallback) Generate(ctx context.Context, in ReportInput) (ReportFields, string, string, error) {
	if len(f.Models) == 0 {
		return ReportFields{}, "", "", ErrNoModel
	}
	var errs []error
	for _, m := range f.Models {
		tctx, cancel := context.WithTimeout(ctx, f.PerTry)
		fields, raw, err := m.Generate(tctx, in)
		cancel()
		// "Service is too busy" and rate limits are usually gone a moment later.
		if err != nil && busy(err) && ctx.Err() == nil {
			select {
			case <-time.After(f.RetryWait):
			case <-ctx.Done():
			}
			tctx, cancel = context.WithTimeout(ctx, f.PerTry)
			fields, raw, err = m.Generate(tctx, in)
			cancel()
		}
		if err == nil {
			return fields, raw, m.Name(), nil
		}
		if f.OnError != nil {
			f.OnError(m.Name(), err)
		}
		errs = append(errs, err)
		if ctx.Err() != nil {
			break
		}
	}
	return ReportFields{}, "", "", errors.Join(errs...)
}

func busy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "HTTP 503") || strings.Contains(msg, "HTTP 429")
}
