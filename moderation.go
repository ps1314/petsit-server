package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Every photo and video is checked by a vision model before it is kept: sitters send these to
// pet owners, so pornographic, violent or otherwise illegal content must not pass through.
// A video is judged by its cover and frames from its start, middle and end.

const moderationPrompt = `你是内容安全审核员，审核上门喂宠的宠托师拍的照片（可能是一段视频里截出的几帧）。
判断这些图片里有没有以下违规内容：色情或裸露、性暗示、血腥暴力、虐待动物、毒品或违禁品、赌博、政治敏感、恐怖或极端内容。
宠物、食盆、水碗、猫砂、宠物粪便或呕吐物、宠物生病或受伤的记录、家里的环境，都是正常内容，不算违规。
只输出一个 JSON 对象：{"safe": true 或 false, "category": "违规类别，没有就空字符串", "reason": "不超过 20 字的说明，没有就空字符串"}`

type moderationResult struct {
	Safe     *bool  `json:"safe"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// Moderate checks one item's images. It returns ReviewOK or ReviewBlocked with a short reason;
// an error (no vision model, all models down) leaves the decision for later.
func Moderate(ctx context.Context, f *Fallback, images [][]byte) (string, string, error) {
	if len(images) == 0 {
		return "", "", errors.New("nothing to check")
	}
	var res moderationResult
	_, _, err := f.Do(ctx, ChatRequest{Prompt: moderationPrompt, Images: images, JSON: true, Temperature: 0}, func(raw string) error {
		res = moderationResult{}
		if err := json.Unmarshal([]byte(jsonObject(raw)), &res); err != nil || res.Safe == nil {
			return fmt.Errorf("moderation reply is not the expected JSON: %.200s", raw)
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if *res.Safe {
		return ReviewOK, "", nil
	}
	reason := strings.TrimSpace(res.Reason)
	if reason == "" {
		reason = strings.TrimSpace(res.Category)
	}
	if reason == "" {
		reason = "内容不合规"
	}
	if r := []rune(reason); len(r) > 40 {
		reason = string(r[:40])
	}
	return ReviewBlocked, reason, nil
}

// FrameGrabber pulls a few still frames out of a video.
type FrameGrabber func(ctx context.Context, video []byte) ([][]byte, error)

// FFmpegFrames returns a grabber backed by the ffmpeg and ffprobe binaries, or nil when they are
// not installed; without it a video is judged by its cover alone.
func FFmpegFrames() FrameGrabber {
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		return nil
	}
	return func(ctx context.Context, video []byte) ([][]byte, error) {
		dir, err := os.MkdirTemp("", "frames")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		in := filepath.Join(dir, "in")
		if err := os.WriteFile(in, video, 0o600); err != nil {
			return nil, err
		}
		out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", in).Output()
		if err != nil {
			return nil, fmt.Errorf("ffprobe: %w", err)
		}
		dur, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		if err != nil || dur <= 0 {
			return nil, fmt.Errorf("ffprobe: no duration in %q", out)
		}
		var frames [][]byte
		for i, at := range []float64{min(0.5, dur/10), dur / 2, max(dur-0.5, dur*0.9)} {
			name := filepath.Join(dir, fmt.Sprintf("f%d.jpg", i))
			cmd := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-ss", strconv.FormatFloat(at, 'f', 2, 64), "-i", in,
				"-frames:v", "1", "-vf", "scale=480:-2", "-q:v", "5", "-y", name)
			if err := cmd.Run(); err != nil {
				continue
			}
			if b, err := os.ReadFile(name); err == nil && len(b) > 0 {
				frames = append(frames, b)
			}
		}
		if len(frames) == 0 {
			return nil, errors.New("ffmpeg: no frame extracted")
		}
		return frames, nil
	}
}

// Too-large files are refused with this reason rather than left pending forever.
const reasonTooLarge = "文件太大，视频请录短一点"

func tooLarge(err error) bool {
	return err != nil && (errors.Is(err, ErrTooLarge) || strings.Contains(err.Error(), ErrTooLarge.Error()))
}

// review fetches an item and runs the content check. A failure to fetch, to pull frames out of
// a video or to reach any model returns ReviewPending, so the item waits instead of passing
// unchecked. A video is never passed on its cover alone: the cover is whatever image the app
// sent, while the frames come from the video itself.
func (a *API) review(ctx context.Context, p Photo) (string, string) {
	if a.ReviewLimit != nil && !a.ReviewLimit.Allow(p.sitterID) {
		return ReviewPending, ""
	}
	fetch := func(id string) ([]byte, string) {
		b, err := a.Photos.Fetch(ctx, []string{id})
		if tooLarge(err) {
			return nil, ReviewBlocked
		}
		if err != nil || len(b) == 0 {
			a.Log.Warn("review: fetch", "file", id, "err", err)
			return nil, ReviewPending
		}
		return b[0], ""
	}
	var images [][]byte
	media, verdict := fetch(p.FileID)
	if verdict != "" {
		return verdict, map[string]string{ReviewBlocked: reasonTooLarge}[verdict]
	}
	if p.Kind == KindVideo {
		if a.Frames == nil {
			a.Log.Warn("review: no frame grabber, video waits", "file", p.FileID)
			return ReviewPending, ""
		}
		frames, err := a.Frames(ctx, media)
		if err != nil || len(frames) == 0 {
			a.Log.Warn("review: frames", "file", p.FileID, "err", err)
			return ReviewPending, ""
		}
		images = append(images, frames...)
		if p.ThumbFileID != "" {
			cover, verdict := fetch(p.ThumbFileID)
			if verdict != "" {
				return verdict, map[string]string{ReviewBlocked: reasonTooLarge}[verdict]
			}
			images = append(images, cover)
		}
	} else {
		if len(media) > maxImageBytes {
			return ReviewBlocked, reasonTooLarge
		}
		images = append(images, media)
	}
	verdict, reason, err := Moderate(ctx, a.Model, images)
	if err != nil {
		a.Log.Warn("review: model", "file", p.FileID, "err", err)
		return ReviewPending, ""
	}
	return verdict, reason
}

// reviewPending retries the check for items that could not be checked on upload; it runs before
// a report so nothing unchecked reaches the owner.
func (a *API) reviewPending(ctx context.Context, photos []Photo) []Photo {
	for i, p := range photos {
		if p.Review != ReviewPending {
			continue
		}
		verdict, reason := a.review(ctx, p)
		if verdict == ReviewPending {
			continue
		}
		if err := a.Store.SetReview(ctx, p.ID, verdict, reason); err != nil {
			a.Log.Error("review: save", "photo", p.ID, "err", err)
			continue
		}
		photos[i].Review, photos[i].ReviewReason = verdict, reason
	}
	return photos
}

const reviewTimeout = 50 * time.Second
