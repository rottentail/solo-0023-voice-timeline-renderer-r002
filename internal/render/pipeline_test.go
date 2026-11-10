package render

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

func sineWav(t *testing.T, sampleRate, channels, frames int, freq, amp float64) []byte {
	t.Helper()
	samples := make([]float32, frames*channels)
	for f := 0; f < frames; f++ {
		seconds := float64(f) / float64(sampleRate)
		first := float32(math.Sin(2*math.Pi*freq*seconds) * amp)
		if channels == 1 {
			samples[f] = first
			continue
		}
		samples[f*2] = first
		samples[f*2+1] = first * 0.8
	}
	b, err := audiocodec.EncodePCM16(sampleRate, channels, samples)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type memLoader map[string]SourcePayload

func (m memLoader) Load(_ context.Context, id string) (SourcePayload, error) { return m[id], nil }

type memSink struct{ dir string }

func (s *memSink) NewOutput(sampleRate, channels int) (*FileOutput, error) {
	f, err := os.CreateTemp(s.dir, "out-*.wav")
	if err != nil {
		return nil, err
	}
	return NewFileOutput(f, sampleRate, channels)
}
func (s *memSink) Commit(*FileOutput, domain.RenderReceipt) error { return nil }
func (s *memSink) Abort(o *FileOutput) {
	if o != nil {
		o.CloseFile()
	}
}

func TestPipelineEndToEnd(t *testing.T) {
	dir := t.TempDir()
	w1 := sineWav(t, 48000, 1, 19200, 220, 0.25)
	w2 := sineWav(t, 44100, 2, 15435, 330, 0.2)
	loader := memLoader{
		"a": {Asset: domain.SourceAsset{ID: "a", Digest: "x", SampleRate: 48000, Channels: 1, FrameCount: 19200}, WAV: w1},
		"b": {Asset: domain.SourceAsset{ID: "b", Digest: "y", SampleRate: 44100, Channels: 2, FrameCount: 15435}, WAV: w2},
	}
	plan := domain.Timeline{
		ID: "tl", Name: "t", SampleRate: 48000, OutputChannels: 2, TargetPeakDBFS: -1,
		Spans: []domain.TimelineSpan{
			{SourceID: "a", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
			{SourceID: "b", SourceStartFrame: 0, SourceEndFrame: 15435, GainDB: 0, CrossfadeFrames: 4800},
		},
	}
	out, _, err := Render(context.Background(), "r", plan, loader, &memSink{dir: dir}, Limits{BlockFrames: 32768, ResidentBlocks: 64}, dir, time.Now())
	if err != nil {
		t.Fatalf("render: %+v", err)
	}
	t.Logf("output frames=%d bytes=%d digest=%s", out.FrameCount, out.ByteCount, out.Digest)
}
