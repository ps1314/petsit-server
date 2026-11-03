package main

import (
	"regexp"
	"strings"
)

// Text bound for a model passes through maskText first: owners' messages, and everything sitters
// type (report edits, notes) that later feeds history and summaries. Phone numbers, WeChat IDs,
// door and room numbers and lock or pickup codes must never reach a model.
var (
	rePhone  = regexp.MustCompile(`(?:\+?86[- .]?)?1[3-9]\d[- .]?\d{4}[- .]?\d{4}|0\d{2,3}[- .]?\d{7,8}`)
	reWechat = regexp.MustCompile(`(?i)(微信号码|微信号|微信id|微信|wechat|weixin|vx号|vx|wx|v信|威信|加v)\s*(?:是|为|[:：])?\s*([a-z][-_a-z0-9]{5,19})`)
	reSecret = regexp.MustCompile(`(密码|门锁|门禁码|门禁卡|门禁|取件码|开门码|锁)(?:是|为|[:：\s]){0,4}[\d#*][\d#*\s]{2,}`)
	reDoor   = regexp.MustCompile(`[A-Za-z]\s*座\s*\d*|\d+\s*(栋|幢|号楼|座|单元|室|号房|层|楼)|\d{1,3}-\d{3,4}(-\d{1,4})?`)
	// A room number often trails the building with no unit word: 3栋1203.
	reDoorTail = regexp.MustCompile(`(\[门牌\][\s-]*)+\d*(\s*(室|号|房))?`)
	reDigits   = regexp.MustCompile(`\d{3,}`)
)

// halfWidth folds full-width digits, letters and the few symbols the patterns rely on.
func halfWidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= '０' && r <= '９', r >= 'Ａ' && r <= 'Ｚ', r >= 'ａ' && r <= 'ｚ':
			return r - '０' + '0'
		case r == '－':
			return '-'
		case r == '＃':
			return '#'
		case r == '．':
			return '.'
		case r == '＊':
			return '*'
		case r == '　':
			return ' '
		}
		return r
	}, s)
}

func maskText(text string) string {
	text = halfWidth(text)
	text = rePhone.ReplaceAllString(text, "[电话]")
	text = reWechat.ReplaceAllString(text, "$1[微信号]")
	text = reSecret.ReplaceAllString(text, "$1[密码]")
	text = reDoor.ReplaceAllString(text, "[门牌]")
	text = reDoorTail.ReplaceAllString(text, "[门牌]")
	// Anything else with three or more digits could be a code or a room; only a year survives.
	return maskDigits(text)
}

func maskDigits(text string) string {
	var b strings.Builder
	last := 0
	for _, m := range reDigits.FindAllStringIndex(text, -1) {
		run := text[m[0]:m[1]]
		after := text[m[1]:]
		if len(run) == 4 && (strings.HasPrefix(after, "年") || strings.HasPrefix(after, "-") || strings.HasPrefix(after, ".") || strings.HasPrefix(after, "/")) && run >= "2000" && run <= "2099" {
			continue
		}
		b.WriteString(text[last:m[0]])
		b.WriteString("[数字]")
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String()
}

func (f ReportFields) masked() ReportFields {
	f.Food, f.Water, f.Litter, f.Mood = maskText(f.Food), maskText(f.Water), maskText(f.Litter), maskText(f.Mood)
	f.Anomaly, f.Note, f.Alert, f.OwnerMsg = maskText(f.Anomaly), maskText(f.Note), maskText(f.Alert), maskText(f.OwnerMsg)
	return f
}

// Words that turn an observation into a diagnosis. The prompts forbid diagnosing; sentences that
// slip through anyway are dropped before anything is shown or saved.
var diagnosisWords = []string{"诊断", "确诊", "病症", "疾病", "生病", "患有", "患上", "感染", "发炎", "炎症", "肠胃炎", "膀胱炎",
	"肾衰", "肾病", "结石", "糖尿病", "中毒", "肿瘤", "癌", "猫瘟", "犬瘟", "细小病毒", "传腹", "杯状", "疱疹", "寄生虫", "综合征"}

var reSentence = regexp.MustCompile(`[^。！？!?；;\n]+[。！？!?；;\n]?`)

func stripDiagnosis(s string) string {
	var keep []string
	for _, sent := range reSentence.FindAllString(s, -1) {
		bad := false
		for _, w := range diagnosisWords {
			if strings.Contains(sent, w) {
				bad = true
				break
			}
		}
		if !bad {
			keep = append(keep, sent)
		}
	}
	return strings.TrimSpace(strings.Join(keep, ""))
}

// undiagnosed drops diagnosing sentences from the AI-written fields; an alert left empty becomes
// 无 and takes its owner message with it.
func (f ReportFields) undiagnosed() ReportFields {
	f.Anomaly, f.Alert, f.OwnerMsg, f.Note = stripDiagnosis(f.Anomaly), stripDiagnosis(f.Alert), stripDiagnosis(f.OwnerMsg), stripDiagnosis(f.Note)
	f.Mood = stripDiagnosis(f.Mood)
	return f.normalized()
}
