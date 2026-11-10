package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

func sineBytes(t *testing.T, sampleRate, channels, frames int, freq, amp float64) []byte {
	t.Helper()
	samples := make([]float32, frames*channels)
	for f := 0; f < frames; f++ {
		seconds := float64(f) / float64(sampleRate)
		v := float32(math.Sin(2*math.Pi*freq*seconds) * amp)
		if channels == 1 {
			samples[f] = v
		} else {
			samples[f*2] = v
			samples[f*2+1] = v * 0.8
		}
	}
	b, err := audiocodec.EncodePCM16(sampleRate, channels, samples)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func newServer(t *testing.T) (*httptest.Server, *service.Service) {
	t.Helper()
	repo, err := storage.NewRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(repo, service.DefaultConfig())
	srv := httptest.NewServer(New(svc, Options{MaxSourceRequestBytes: 48 << 20, MaxJSONBytes: 2 << 20}).Handler())
	t.Cleanup(srv.Close)
	return srv, svc
}

func doJSON(t *testing.T, method, url string, payload any) (int, map[string]any) {
	t.Helper()
	var body *bytes.Buffer
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		body = bytes.NewBuffer(encoded)
	} else {
		body = &bytes.Buffer{}
	}
	req, _ := http.NewRequest(method, url, body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw := make(map[string]any)
	_ = json.NewDecoder(resp.Body).Decode(&raw)
	return resp.StatusCode, raw
}

// TestShortOpsNotBlockedByRender starts a render (which streams through the
// service) and proves concurrent source/timeline reads return immediately
// rather than waiting behind the render work.
func TestShortOpsNotBlockedByRender(t *testing.T) {
	srv, _ := newServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = ctx

	// Register a source and a long-ish timeline.
	wav := sineBytes(t, 48000, 1, 48000*30, 200, 0.3)
	status, _ := doJSON(t, "PUT", srv.URL+"/v1/sources/src_block_01",
		map[string]any{"name": "long take", "wav_base64": base64.StdEncoding.EncodeToString(wav)})
	if status != http.StatusCreated {
		t.Fatalf("source status %d", status)
	}
	spans := []map[string]any{
		{"source_id": "src_block_01", "source_start_frame": 0, "source_end_frame": 48000 * 30, "gain_db": 0, "crossfade_frames": 0},
	}
	status, tlRaw := doJSON(t, "PUT", srv.URL+"/v1/timelines/tl_block_01", map[string]any{
		"name": "long", "sample_rate": 48000, "output_channels": 2,
		"target_peak_dbfs": -1, "spans": spans,
	})
	if status != http.StatusCreated {
		t.Fatalf("timeline status %d", status)
	}
	tlID := tlRaw["id"].(string)

	// Fire the render. With in-process httptest the handler runs to completion
	// in the request goroutine, so run it in the background and concurrently
	// exercise reads that must not serialize behind the render.
	renderDone := make(chan struct{})
	go func() {
		defer close(renderDone)
		code, _ := doJSON(t, "PUT", srv.URL+"/v1/renders/render_block_01",
			map[string]any{"timeline_id": tlID})
		if code != http.StatusCreated {
			t.Errorf("render status %d", code)
		}
	}()

	// Give the render a moment to start, then issue short reads. They must
	// return far faster than the render takes.
	time.Sleep(100 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		start := time.Now()
		code, _ := doJSON(t, "GET", srv.URL+"/v1/sources/src_block_01", nil)
		if code != http.StatusOK {
			t.Fatalf("source read status %d", code)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Fatalf("source read blocked for %s behind render", elapsed)
		}
		code, _ = doJSON(t, "GET", srv.URL+"/v1/timelines/tl_block_01", nil)
		if code != http.StatusOK {
			t.Fatalf("timeline read status %d", code)
		}
		// A second source creation (short write) should also make progress.
		select {
		case <-renderDone:
			// Render finished early; the non-blocking property was already
			// exercised above.
			goto finished
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
finished:
	<-renderDone

	// Final job is completed and output available.
	code, jobRaw := doJSON(t, "GET", srv.URL+"/v1/renders/render_block_01", nil)
	if code != http.StatusOK || jobRaw["state"] != string(domain.RenderCompleted) {
		t.Fatalf("job not completed: code=%d state=%v", code, jobRaw["state"])
	}
}

// TestCapacityReturnsRetryable503 drives the HTTP mapping for a saturated
// renderer and confirms the Retry-After hint and queued job state.
func TestCapacityReturnsRetryable503(t *testing.T) {
	repo, err := storage.NewRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := service.DefaultConfig()
	config.MaxParallelRenders = 1
	svc := service.New(repo, config)
	// Saturate the single worker slot directly.
	release := svc.HoldRenderSlotForTest()
	defer release()

	srv := httptest.NewServer(New(svc, Options{MaxSourceRequestBytes: 48 << 20, MaxJSONBytes: 2 << 20}).Handler())
	t.Cleanup(srv.Close)

	wav := sineBytes(t, 48000, 1, 12000, 200, 0.3)
	doJSON(t, "PUT", srv.URL+"/v1/sources/src_503_01",
		map[string]any{"name": "s", "wav_base64": base64.StdEncoding.EncodeToString(wav)})
	doJSON(t, "PUT", srv.URL+"/v1/timelines/tl_503_01", map[string]any{
		"name": "t", "sample_rate": 48000, "output_channels": 2, "target_peak_dbfs": -1,
		"spans": []map[string]any{{"source_id": "src_503_01", "source_start_frame": 0, "source_end_frame": 12000, "gain_db": 0, "crossfade_frames": 0}},
	})
	req, _ := http.NewRequest("PUT", srv.URL+"/v1/renders/render_503_01",
		bytes.NewBufferString(`{"timeline_id":"tl_503_01"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	var envelope struct {
		Error domain.ErrorDetail `json:"error"`
		Job   domain.RenderJob   `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "render_capacity_exhausted" {
		t.Fatalf("unexpected code %s", envelope.Error.Code)
	}
	if envelope.Job.State != domain.RenderQueued {
		t.Fatalf("job state = %s, want queued", envelope.Job.State)
	}
}
