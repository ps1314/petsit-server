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
	// Alert is a change against earlier visits worth the owner's attention; OwnerMsg is a message
	// the sitter can send about it. Neither goes on the image unless the sitter keeps Alert.
	Alert    string `json:"alert"`
	OwnerMsg string `json:"owner_msg"`
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
	f.Alert = strings.TrimSpace(f.Alert)
	if f.Alert == "" {
		f.Alert = "无"
	}
	f.OwnerMsg = strings.TrimSpace(f.OwnerMsg)
	if f.Alert == "无" {
		f.OwnerMsg = ""
	}
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
	// History holds this pet's earlier reports, newest first, for spotting changes.
	History []PastReport
	// NoAlert turns the alert off entirely (AI_FEATURES_OFF=trend).
	NoAlert bool
}

// PastReport is an earlier visit's report for the same client.
type PastReport struct {
	StartAt int64
	Fields  ReportFields
}

var ErrNoModel = errors.New("AI 还没配置，可以先手动填写报告")

const reportPrompt = `你在帮上门喂宠的宠托师给宠物主人写当次上门报告。
只根据照片里能看到的内容写，看不清或照片里没有的项目，必须原样写"照片里看不出来"，不许猜，不许写套话。
宠物：%s
到达时间：%s，离开时间：%s
%s
只输出一个 JSON 对象，字段都是简短中文（除 owner_msg 外每项不超过 40 字）：
{"food":"进食情况，比如食盆剩余多少","water":"饮水情况","litter":"猫砂或便便情况","mood":"精神状态","anomaly":"异常情况，没有就写无","note":"给主人的一句话，语气温和，不超过 30 字","alert":"需要留意的变化，没有就写无","owner_msg":"alert 不是无时，宠托师可以直接发给宠主的一段话；否则写空字符串"}`

// historyPrompt lets the model compare this visit with the pet's last few. It only reports
// changes it has grounds for and never diagnoses: the sitter is not a vet and neither is the model.
const historyPrompt = `
同一只宠物最近几次上门的报告（从近到远）：
%s
把本次照片和这些记录对比，在 alert 里写需要宠主留意的变化，比如连续剩粮、饮水变少、便便异常、呕吐、精神变差、和上次相比明显不同。
只写有依据的变化，用"观察到……"的说法，不做诊断、不说病名；没有值得留意的变化就写"无"。
alert 不是"无"时，owner_msg 写一段宠托师可以直接发给宠主的话：不超过 80 字，平和、具体，说明观察到什么、宠托师接下来会怎么留意，要不要联系宠物医生由宠主决定。
`

const noHistoryPrompt = `
这只宠物还没有之前的上门记录。alert 只在本次照片里有明显需要宠主知道的情况时填写（比如呕吐物、便便明显异常、食物完全没动），用"观察到……"的说法，不做诊断；否则写"无"。owner_msg 规则同上：alert 不是"无"时写一段不超过 80 字、可以直接发给宠主的话，否则写空字符串。
`

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
	if in.NoAlert {
		extra += "\nalert 一律写\"无\"，owner_msg 写空字符串。\n"
	} else if len(in.History) > 0 {
		var lines []string
		for _, h := range in.History {
			f := h.Fields.masked() // sitters type free text into reports
			lines = append(lines, fmt.Sprintf("- %s：进食 %s；饮水 %s；猫砂/便便 %s；精神 %s；异常 %s",
				time.Unix(h.StartAt, 0).In(Shanghai).Format("1月2日"), f.Food, f.Water, f.Litter, f.Mood, f.Anomaly))
		}
		extra += fmt.Sprintf(historyPrompt, strings.Join(lines, "\n"))
	} else {
		extra += noHistoryPrompt
	}
	return fmt.Sprintf(reportPrompt, maskText(in.Pets), clock(in.ArrivedAt), clock(in.LeftAt), extra)
}

// ChatRequest is one prompt, optionally with photos. JSON asks the endpoint for a JSON object.
type ChatRequest struct {
	Prompt      string
	Images      [][]byte
	JSON        bool
	Temperature float64
}

// LLM is one configured model endpoint.
type LLM interface {
	Name() string
	// Vision reports whether the model accepts images; text-only models serve text-only tasks.
	Vision() bool
	Chat(ctx context.Context, req ChatRequest) (string, error)
}

// OpenAICompat talks to any chat completions endpoint (DeepSeek, DashScope compatible mode,
// Doubao). Swapping models is a config change.
type OpenAICompat struct {
	BaseURL  string
	Model    string
	APIKey   string
	TextOnly bool
	HTTP     *http.Client
}

func (m *OpenAICompat) Name() string { return m.Model }
func (m *OpenAICompat) Vision() bool { return !m.TextOnly }

func (m *OpenAICompat) Chat(ctx context.Context, in ChatRequest) (string, error) {
	var content any = in.Prompt
	if len(in.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": in.Prompt}}
		for _, p := range in.Images {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]string{"url": "data:" + http.DetectContentType(p) + ";base64," + base64.StdEncoding.EncodeToString(p)},
			})
		}
		content = parts
	}
	payload := map[string]any{
		"model":       m.Model,
		"messages":    []map[string]any{{"role": "user", "content": content}},
		"temperature": in.Temperature,
	}
	if in.JSON {
		payload["response_format"] = map[string]string{"type": "json_object"}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d: %.300s", m.Model, resp.StatusCode, respBody)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("%s: unexpected response: %.300s", m.Model, respBody)
	}
	return out.Choices[0].Message.Content, nil
}

// jsonObject cuts the outermost {...} out of a reply, tolerating the ```json fence some models
// add even in JSON mode.
func jsonObject(raw string) string {
	s := strings.TrimSpace(raw)
	if i, j := strings.Index(s, "{"), strings.LastIndex(s, "}"); i >= 0 && j > i {
		s = s[i : j+1]
	}
	return s
}

func parseReport(raw string) (ReportFields, error) {
	var f ReportFields
	if err := json.Unmarshal([]byte(jsonObject(raw)), &f); err != nil {
		return f, fmt.Errorf("report is not JSON: %.200s", raw)
	}
	return f.normalized(), nil
}

// GenerateReport writes one visit report from its photos.
func GenerateReport(ctx context.Context, f *Fallback, in ReportInput) (ReportFields, string, string, error) {
	var fields ReportFields
	model, raw, err := f.Do(ctx, ChatRequest{Prompt: buildPrompt(in), Images: in.Photos, JSON: true, Temperature: 0.2}, func(raw string) error {
		var err error
		fields, err = parseReport(raw)
		fields = fields.undiagnosed()
		if in.NoAlert {
			fields.Alert, fields.OwnerMsg = "无", ""
		}
		return err
	})
	return fields, raw, model, err
}
