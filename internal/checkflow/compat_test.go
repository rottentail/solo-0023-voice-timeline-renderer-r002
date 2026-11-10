package checkflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"testing"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// TestCompatibilityPreserved runs the production render workflow through the
// real HTTP handler and asserts source measurements, output digest, peak
// normalization, and receipt conclusions remain internally consistent after the
// concurrency/memory rework.
func TestCompatibilityPreserved(t *testing.T) {
	harness, err := NewHarness()
	if err != nil {
		t.Fatal(err)
	}
	defer harness.Close()
	ctx := context.Background()

	first, err := sineWAV(48000, 1, 400, 220, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sineWAV(44100, 2, 350, 330, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	srcA, status, err := harness.createSource(ctx, "src_compat_a", "opening", first)
	if err != nil || status != 201 {
		t.Fatalf("source A: status=%d err=%v", status, err)
	}
	// Source peak/RMS must still be populated exactly as before.
	if srcA.PeakDBFS == 0 || srcA.RMSDBFS == 0 || srcA.DurationMS != 400 {
		t.Fatalf("source measurements regressed: %+v", srcA)
	}
	if _, status, err := harness.createSource(ctx, "src_compat_b", "closing", second); err != nil || status != 201 {
		t.Fatalf("source B: status=%d err=%v", status, err)
	}
	spans := []domain.TimelineSpan{
		{SourceID: "src_compat_a", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
		{SourceID: "src_compat_b", SourceStartFrame: 0, SourceEndFrame: 15435, GainDB: 0, CrossfadeFrames: 4800},
	}
	plan, status, err := harness.createTimeline(ctx, "tl_compat_01", "compat", 48000, 2, -1, spans)
	if err != nil || status != 201 {
		t.Fatalf("timeline: status=%d err=%v", status, err)
	}
	job, status, err := harness.createRender(ctx, "render_compat_01", plan.ID)
	if err != nil || status != 201 || job.State != domain.RenderCompleted {
		t.Fatalf("render: status=%d state=%s err=%v", status, job.State, err)
	}
	receipt, status, err := harness.getReceipt(ctx, job.ID)
	if err != nil || status != 200 {
		t.Fatalf("receipt: status=%d err=%v", status, err)
	}
	if receipt.SchemaVersion != 1 || receipt.Normalization.RequestedPeakDBFS != -1 {
		t.Fatalf("receipt contract changed: %+v", receipt.Normalization)
	}
	audio, status, err := harness.getOutput(ctx, job.ID)
	if err != nil || status != 200 {
		t.Fatalf("output: status=%d err=%v", status, err)
	}
	digest := sha256.Sum256(audio)
	if hex.EncodeToString(digest[:]) != receipt.Output.Digest {
		t.Fatal("output digest does not match receipt")
	}
	clip, info, err := audiocodec.DecodeWAV(audio)
	if err != nil {
		t.Fatal(err)
	}
	measured := audiocodec.Measure(clip.Samples)
	if math.Abs(measured.PeakDBFS-(-1)) > 0.25 {
		t.Fatalf("normalization target not met: %v", measured.PeakDBFS)
	}
	if info.FrameCount != receipt.Output.FrameCount || info.FrameCount != plan.OutputFrames {
		t.Fatalf("frame count contract changed")
	}

	// Re-rendering the same timeline under a new id yields identical audio bytes
	// (determinism at the HTTP boundary).
	job2, status, err := harness.createRender(ctx, "render_compat_02", plan.ID)
	if err != nil || status != 201 || job2.State != domain.RenderCompleted {
		t.Fatalf("second render: status=%d state=%s", status, job2.State)
	}
	audio2, _, err := harness.getOutput(ctx, job2.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest2 := sha256.Sum256(audio2)
	if digest2 != digest {
		t.Fatal("same timeline produced different audio on repeat render")
	}
}
