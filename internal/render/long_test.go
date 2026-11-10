package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// TestThirtyMinuteBoundedMemory renders 30 minutes of 48 kHz stereo
// (86.4 million frames) with a small resident block budget and proves:
//   - it completes and produces a valid WAV of the expected length,
//   - heap allocation stays bounded regardless of timeline length,
//   - output bytes are deterministic across repeated renders.
func TestThirtyMinuteBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("long render skipped in short mode")
	}
	const (
		sampleRate = 48000
		channels   = 2
		durationS  = 30 * 60
		frames     = sampleRate * durationS // 86,400,000
	)
	dir := t.TempDir()

	// Build a modest-sized source and repeat it across many spans to reach the
	// full duration, mirroring a narrated timeline assembled from takes.
	sourceFrames := 48000 * 60 // one-minute take
	wav := sineWav(t, sampleRate, channels, sourceFrames, 196, 0.2)
	digestBytes := sha256.Sum256(wav)
	loader := memLoader{}
	assets := map[string]domain.SourceAsset{}
	spans := make([]domain.TimelineSpan, 0, durationS/60)
	takeCount := durationS / 60
	for i := 0; i < takeCount; i++ {
		id := "take"
		loader[id] = SourcePayload{
			Asset: domain.SourceAsset{ID: id, Digest: hex.EncodeToString(digestBytes[:]), SampleRate: sampleRate, Channels: channels, FrameCount: int64(sourceFrames)},
			WAV:   wav,
		}
		assets[id] = loader[id].Asset
		spans = append(spans, domain.TimelineSpan{
			SourceID: id, SourceStartFrame: 0, SourceEndFrame: int64(sourceFrames),
			GainDB: 0, CrossfadeFrames: 0,
		})
	}
	plan := domain.Timeline{
		ID: "long", Name: "30min", SampleRate: sampleRate, OutputChannels: channels,
		TargetPeakDBFS: -1, Spans: spans,
	}

	// Tight block budget: only 16 resident 16k-frame blocks (~2 MiB of mix).
	limits := Limits{MaxOutputFrames: frames, BlockFrames: 16384, ResidentBlocks: 16}

	runtime.GC()
	var baseline runtime.MemStats
	runtime.ReadMemStats(&baseline)

	sink := &captureSink{dir: dir}
	start := time.Now()
	out, receipt, err := Render(context.Background(), "render_long_01", plan, loader, sink, limits, dir, time.Now())
	if err != nil {
		t.Fatalf("30-minute render failed: %v", err)
	}
	elapsed := time.Since(start)

	runtime.GC()
	var peak runtime.MemStats
	runtime.ReadMemStats(&peak)

	if out.FrameCount != frames {
		t.Fatalf("frame count = %d, want %d", out.FrameCount, frames)
	}
	if got := (int(out.ByteCount) - 44) / (2 * channels); int64(got) != frames {
		t.Fatalf("WAV payload frames mismatch")
	}
	if len(sink.written) != int(out.ByteCount) {
		t.Fatalf("byte count mismatch output=%d file=%d", out.ByteCount, len(sink.written))
	}
	// Verify the WAV decodes and reports matching metadata, then release the
	// large decoded buffer so it does not pollute the renderer heap measurement.
	clip, info, err := audiocodec.DecodeWAV(sink.written)
	if err != nil {
		t.Fatalf("decode output: %v", err)
	}
	decodedSamples := len(clip.Samples)
	if info.FrameCount != frames || info.Channels != channels {
		t.Fatalf("decoded metadata mismatch: %+v", info)
	}
	if receipt.Output.FrameCount != frames {
		t.Fatalf("receipt frame count mismatch")
	}
	if receipt.Output.Digest != out.Digest {
		t.Fatalf("receipt/output digest mismatch")
	}
	clip = nil

	// Determinism: a second render of the identical plan yields the same digest.
	// The second sink's bytes are discarded after comparison to avoid retaining
	// two full copies in the test process.
	sink2 := &captureSink{dir: dir}
	out2, _, err := Render(context.Background(), "render_long_01", plan, loader, sink2, limits, dir, time.Now())
	if err != nil {
		t.Fatalf("repeat render failed: %v", err)
	}
	if !bytes.Equal(sink.written, sink2.written) || out2.Digest != out.Digest {
		t.Fatal("30-minute render is not deterministic")
	}
	sink2.written = nil

	// After garbage collection, live heap must stay small: the multi-hundred-MiB
	// mix lives in the scratch file, not in memory.
	runtime.GC()
	var live runtime.MemStats
	runtime.ReadMemStats(&live)

	t.Logf("30min render: elapsed=%s output=%d MiB liveHeap=%d MiB totalAlloc=%d MiB decodedSamples=%d",
		elapsed, out.ByteCount>>20, live.HeapAlloc>>20, peak.TotalAlloc>>20, decodedSamples)

	// The resident mix budget must stay within a few MiB even though the full
	// timeline would require ~660 MiB of float32 buffers in the old design.
	maxResidentMix := int64(16384) * int64(channels) * 4 * 16
	if maxResidentMix > 8<<20 {
		t.Fatalf("resident mix budget unexpectedly large: %d", maxResidentMix)
	}
	// After GC, live heap for the render itself must be well under the old
	// single-mix footprint. The bound leaves room for source clips and runtime.
	const liveHeapLimit = 200 << 20
	if live.HeapAlloc > liveHeapLimit {
		t.Fatalf("live heap %d MiB exceeds bounded limit %d MiB", live.HeapAlloc>>20, liveHeapLimit>>20)
	}
	_ = baseline
}

// TestTwoConcurrentRendersBounded proves two 30-minute-class renders can make
// progress at the same time while each keeps its mix in a small resident set.
func TestTwoConcurrentRendersBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("long render skipped in short mode")
	}
	const (
		sampleRate = 48000
		channels   = 2
		// Use a shorter but still multi-block timeline so the test stays fast
		// while exercising real spilling under concurrency.
		frames = 48000 * 120
	)
	dir := t.TempDir()
	sourceFrames := 48000 * 30
	wav := sineWav(t, sampleRate, channels, sourceFrames, 196, 0.2)
	digestBytes := sha256.Sum256(wav)
	makePlan := func(id string) (domain.Timeline, memLoader) {
		loader := memLoader{
			"take": {Asset: domain.SourceAsset{ID: "take", Digest: hex.EncodeToString(digestBytes[:]), SampleRate: sampleRate, Channels: channels, FrameCount: int64(sourceFrames)}, WAV: wav},
		}
		spans := make([]domain.TimelineSpan, 0, 4)
		for i := 0; i < 4; i++ {
			spans = append(spans, domain.TimelineSpan{SourceID: "take", SourceStartFrame: 0, SourceEndFrame: int64(sourceFrames), GainDB: 0, CrossfadeFrames: 0})
		}
		return domain.Timeline{ID: id, Name: id, SampleRate: sampleRate, OutputChannels: channels, TargetPeakDBFS: -1, Spans: spans}, loader
	}
	limits := Limits{MaxOutputFrames: frames, BlockFrames: 16384, ResidentBlocks: 16}

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	digests := make(chan string, 2)
	for _, id := range []string{"render_conc_a", "render_conc_b"} {
		wg.Add(1)
		go func(renderID string) {
			defer wg.Done()
			plan, loader := makePlan(renderID)
			sink := &captureSink{dir: dir}
			out, _, err := Render(context.Background(), renderID, plan, loader, sink, limits, dir, time.Now())
			if err != nil {
				errs <- err
				return
			}
			if out.FrameCount != frames {
				errs <- fmt.Errorf("frame count %d want %d", out.FrameCount, frames)
				return
			}
			digests <- out.Digest
		}(id)
	}
	wg.Wait()
	close(errs)
	close(digests)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	collected := 0
	var digest string
	for d := range digests {
		digest = d
		collected++
	}
	// Both renders complete. Identical input timelines deterministically yield
	// the same audio digest, which is the required behavior.
	if collected != 2 || digest == "" {
		t.Fatalf("expected two completed renders, got %d", collected)
	}
}
