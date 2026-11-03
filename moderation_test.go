package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// idFetcher returns each file's own id as its content, so a fake model can tell files apart.
type idFetcher struct{}

func (idFetcher) Fetch(_ context.Context, ids []string) ([][]byte, error) {
	out := make([][]byte, len(ids))
	for i, id := range ids {
		out[i] = append([]byte{0xff, 0xd8, 0xff, 0xe0}, id...)
	}
	return out, nil
}

// censor flags any image whose bytes mention "bad"; down makes every call fail.
type censor struct {
	down    bool
	checked int
}

func (c *censor) Name() string { return "censor" }
func (c *censor) Vision() bool { return true }
func (c *censor) Chat(_ context.Context, req ChatRequest) (string, error) {
	if c.down {
		return "", errors.New("HTTP 500: down")
	}
	if !strings.Contains(req.Prompt, "内容安全审核员") {
		return `{"food":"吃完了"}`, nil
	}
	c.checked++
	for _, img := range req.Images {
		if bytes.Contains(img, []byte("bad")) {
			return `{"safe":false,"category":"色情","reason":"含裸露内容"}`, nil
		}
	}
	return `{"safe":true}`, nil
}

func modEnv(t *testing.T) (*testEnv, *censor, string) {
	c := &censor{}
	e := newEnv(t, c)
	e.api.Photos = idFetcher{}
	visit := e.onboard("o1")
	e.must(200, "o1", "POST", visit+"/arrive", "")
	return e, c, visit
}

func photoOf(out map[string]any) map[string]any { return out["photo"].(map[string]any) }

func TestUploadsAreReviewed(t *testing.T) {
	e, c, visit := modEnv(t)
	ok := e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/cat.jpg"}`)
	if p := photoOf(ok); p["review"] != ReviewOK || p["kind"] != KindImage {
		t.Fatalf("photo = %v", p)
	}
	out := e.must(422, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/bad.jpg"}`)
	if out["blocked"] != true || !strings.Contains(out["error"].(string), "含裸露内容") {
		t.Fatalf("blocked reply = %v", out)
	}
	if n := len(e.must(200, "o1", "GET", visit, "")["photos"].([]any)); n != 1 {
		t.Fatalf("blocked photo was kept: %d photos", n)
	}
	if c.checked != 2 {
		t.Fatalf("checked %d uploads, want 2", c.checked)
	}
}

func TestVideoReviewedByFramesAndCover(t *testing.T) {
	e, _, visit := modEnv(t)
	var grabbed int
	e.api.Frames = func(_ context.Context, video []byte) ([][]byte, error) {
		grabbed++
		return [][]byte{video, video}, nil // frames carry the video's content
	}
	v := e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/v1.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/v1.jpg"}`)
	if p := photoOf(v); p["review"] != ReviewOK || p["kind"] != KindVideo || grabbed != 1 {
		t.Fatalf("video = %v, frames grabbed %d", p, grabbed)
	}
	// Bad content only in the middle of the video, with an innocent cover.
	e.must(422, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/bad.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/cover.jpg"}`)
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/v2.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/v2.jpg"}`)
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/v3.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/v3.jpg"}`)
	e.must(400, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/v4.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/v4.jpg"}`)
	// Cover outside the visit's folder, a cover on a photo, an unknown kind.
	e.must(400, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/v5.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/2/x.jpg"}`)
	e.must(400, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/p.jpg","thumb_file_id":"cloud://env.b/visits/1/x.jpg"}`)
	e.must(400, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/p.gif","kind":"gif"}`)
	// The report reads video covers, never the video file.
	e.must(200, "o1", "POST", visit+"/report", "")
}

func TestVideoNeverPassesOnCoverAlone(t *testing.T) {
	e, _, visit := modEnv(t)
	body := `{"file_id":"cloud://env.b/visits/1/v.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/v.jpg"}`
	if p := photoOf(e.must(201, "o1", "POST", visit+"/photos", body)); p["review"] != ReviewPending {
		t.Fatalf("no frame grabber: %v", p)
	}
	e.api.Frames = func(context.Context, []byte) ([][]byte, error) { return nil, errors.New("moov atom not found") }
	if p := photoOf(e.must(201, "o1", "POST", visit+"/photos", body)); p["review"] != ReviewPending {
		t.Fatalf("frames failed: %v", p)
	}
}

// sizedFetcher serves files of a given size, or ErrTooLarge past the download cap.
type sizedFetcher struct{ size int }

func (f sizedFetcher) Fetch(_ context.Context, ids []string) ([][]byte, error) {
	if f.size > maxMediaBytes {
		return nil, ErrTooLarge
	}
	return [][]byte{bytes.Repeat([]byte{1}, f.size)}, nil
}

func TestOversizedUploadsRefused(t *testing.T) {
	e, _, visit := modEnv(t)
	e.api.Photos = sizedFetcher{maxImageBytes + 1}
	out := e.must(422, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/huge.jpg"}`)
	if !strings.Contains(out["error"].(string), "文件太大") {
		t.Fatalf("huge photo: %v", out)
	}
	e.api.Photos = sizedFetcher{maxMediaBytes + 1}
	e.api.Frames = func(_ context.Context, v []byte) ([][]byte, error) { return [][]byte{v}, nil }
	e.must(422, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/long.mp4","kind":"video","thumb_file_id":"cloud://env.b/visits/1/long.jpg"}`)
}

func TestReadCappedRefusesTruncation(t *testing.T) {
	if _, err := readCapped(bytes.NewReader(make([]byte, maxMediaBytes+1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if b, err := readCapped(bytes.NewReader(make([]byte, 10))); err != nil || len(b) != 10 {
		t.Fatalf("small read %d %v", len(b), err)
	}
}

func TestReviewQuota(t *testing.T) {
	e, c, visit := modEnv(t)
	e.api.ReviewLimit = NewDailyLimit(1)
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/a.jpg"}`)
	if p := photoOf(e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/b.jpg"}`)); p["review"] != ReviewPending || c.checked != 1 {
		t.Fatalf("over quota: %v checked %d", p, c.checked)
	}
}

func TestVideoWithoutFramesOrCoverWaits(t *testing.T) {
	e, _, visit := modEnv(t)
	p := photoOf(e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/v.mp4","kind":"video"}`))
	if p["review"] != ReviewPending {
		t.Fatalf("unchecked video = %v", p)
	}
}

func TestPendingReviewedBeforeReport(t *testing.T) {
	e, c, visit := modEnv(t)
	c.down = true
	for _, f := range []string{"cat", "bad"} {
		if p := photoOf(e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/`+f+`.jpg"}`)); p["review"] != ReviewPending {
			t.Fatalf("%s while models down = %v", f, p)
		}
	}
	e.must(400, "o1", "POST", visit+"/report", "") // nothing reviewed yet
	c.down = false
	out := e.must(200, "o1", "POST", visit+"/report", "")
	reviews := map[string]any{}
	for _, p := range out["photos"].([]any) {
		m := p.(map[string]any)
		reviews[m["file_id"].(string)] = m["review"]
	}
	if reviews["cloud://env.b/visits/1/cat.jpg"] != ReviewOK || reviews["cloud://env.b/visits/1/bad.jpg"] != ReviewBlocked {
		t.Fatalf("reviews after report = %v", reviews)
	}
	firsts, _ := e.api.Store.FirstPhotos(t.Context(), []int64{1})
	if firsts[1].FileID != "cloud://env.b/visits/1/cat.jpg" {
		t.Fatalf("summary photo = %v", firsts[1])
	}
}

func TestOnlyBlockedPhotosCannotReport(t *testing.T) {
	e, c, visit := modEnv(t)
	c.down = true
	e.must(201, "o1", "POST", visit+"/photos", `{"file_id":"cloud://env.b/visits/1/bad.jpg"}`)
	c.down = false
	e.must(400, "o1", "POST", visit+"/report", "")
}

func TestFFmpegFrames(t *testing.T) {
	grab := FFmpegFrames()
	if grab == nil {
		t.Skip("ffmpeg not installed")
	}
	ffmpeg, _ := exec.LookPath("ffmpeg")
	path := filepath.Join(t.TempDir(), "v.mp4")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=3:size=320x240:rate=10", "-pix_fmt", "yuv420p", path).CombinedOutput(); err != nil {
		t.Fatalf("make video: %v %s", err, out)
	}
	video, _ := os.ReadFile(path)
	frames, err := grab(t.Context(), video)
	if err != nil || len(frames) != 3 {
		t.Fatalf("frames %d err %v", len(frames), err)
	}
	for _, f := range frames {
		if !bytes.HasPrefix(f, []byte{0xff, 0xd8}) {
			t.Fatal("frame is not a JPEG")
		}
	}
	if _, err := grab(t.Context(), []byte("not a video")); err == nil {
		t.Fatal("garbage accepted as video")
	}
}
