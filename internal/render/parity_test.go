package render

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"os"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// referenceRender reproduces the pre-optimization in-memory pipeline exactly:
// full-slice prepare, full-buffer mix, in-place normalize, EncodePCM16, then a
// decode+measure for the receipt. It exists only to prove the streaming engine
// is byte-for-byte compatible.
func referenceRender(t *testing.T, plan domain.Timeline, wavs map[string][]byte, assets map[string]domain.SourceAsset) []byte {
	t.Helper()
	decoded := map[string]*audiocodec.Clip{}
	type refSpan struct {
		clip            *audiocodec.Clip
		start           int64
		crossfadeFrames int64
		gain            float64
	}
	refs := make([]refSpan, 0, len(plan.Spans))
	cursor := int64(0)
	for index, span := range plan.Spans {
		clip, ok := decoded[span.SourceID]
		if !ok {
			c, _, err := audiocodec.DecodeWAV(wavs[span.SourceID])
			if err != nil {
				t.Fatal(err)
			}
			clip = c
			decoded[span.SourceID] = c
		}
		selected, err := audiocodec.SliceFrames(context.Background(), clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			t.Fatal(err)
		}
		resampled, err := audiocodec.Resample(context.Background(), selected, plan.SampleRate)
		if err != nil {
			t.Fatal(err)
		}
		mapped, _, err := audiocodec.MapChannels(context.Background(), resampled, plan.OutputChannels)
		if err != nil {
			t.Fatal(err)
		}
		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		cursor = start + mapped.FrameCount()
		refs = append(refs, refSpan{clip: mapped, start: start, crossfadeFrames: span.CrossfadeFrames, gain: span.GainDB})
	}
	channels := plan.OutputChannels
	mix := make([]float32, int(cursor)*channels)
	for _, span := range refs {
		gain := float32(audiocodec.DBToLinear(span.gain))
		frames := span.clip.FrameCount()
		for frame := int64(0); frame < frames; frame++ {
			incoming, outgoing := float32(1), float32(1)
			if span.crossfadeFrames > 0 && frame < span.crossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(span.crossfadeFrames)
				angle := progress * math.Pi / 2
				incoming = float32(math.Sin(angle))
				outgoing = float32(math.Cos(angle))
			}
			inStart := int(frame) * channels
			outStart := int(span.start+frame) * channels
			for ch := 0; ch < channels; ch++ {
				value := span.clip.Samples[inStart+ch] * gain * incoming
				if span.crossfadeFrames > 0 && frame < span.crossfadeFrames {
					mix[outStart+ch] *= outgoing
				}
				mix[outStart+ch] += value
			}
		}
	}
	// reference normalizePeak
	before := audiocodec.Measure(mix)
	target := audiocodec.DBToLinear(plan.TargetPeakDBFS)
	peaks := 0.0
	for _, s := range mix {
		if v := math.Abs(float64(s)); v > peaks {
			peaks = v
		}
	}
	gain := 1.0
	if peaks > 0 {
		gain = target / peaks
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	for i, s := range mix {
		v := float64(s) * gain
		if v > 1 {
			v = 1
		}
		if v < -1 {
			v = -1
		}
		mix[i] = float32(v)
	}
	encoded, err := audiocodec.EncodePCM16(plan.SampleRate, channels, mix)
	if err != nil {
		t.Fatal(err)
	}
	_ = before
	return encoded
}

type captureSink struct {
	dir     string
	written []byte
}

func (s *captureSink) NewOutput(sampleRate, channels int) (*FileOutput, error) {
	f, err := os.CreateTemp(s.dir, "out-*.wav")
	if err != nil {
		return nil, err
	}
	return NewFileOutput(f, sampleRate, channels)
}
func (s *captureSink) Commit(o *FileOutput, _ domain.RenderReceipt) error {
	if err := o.Close(); err != nil {
		return err
	}
	data, err := os.ReadFile(o.File().Name())
	if err != nil {
		return err
	}
	s.written = data
	return nil
}
func (s *captureSink) Abort(o *FileOutput) {
	if o != nil {
		o.CloseFile()
	}
}

func TestStreamingParity(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name       string
		rate       int
		channels   int
		sources    []sourceSpec
		spans      []domain.TimelineSpan
		targetPeak float64
	}{
		{
			name: "crossfaded mixed rates",
			rate: 48000, channels: 2, targetPeak: -1,
			sources: []sourceSpec{
				{id: "a", rate: 48000, ch: 1, frames: 19200, freq: 220, amp: 0.25},
				{id: "b", rate: 44100, ch: 2, frames: 15435, freq: 330, amp: 0.2},
			},
			spans: []domain.TimelineSpan{
				{SourceID: "a", SourceStartFrame: 0, SourceEndFrame: 19200, GainDB: -2, CrossfadeFrames: 0},
				{SourceID: "b", SourceStartFrame: 0, SourceEndFrame: 15435, GainDB: 0, CrossfadeFrames: 4800},
			},
		},
		{
			name: "mono output stereo input",
			rate: 48000, channels: 1, targetPeak: -6,
			sources: []sourceSpec{
				{id: "a", rate: 48000, ch: 2, frames: 24000, freq: 180, amp: 0.5},
			},
			spans: []domain.TimelineSpan{
				{SourceID: "a", SourceStartFrame: 100, SourceEndFrame: 20000, GainDB: 3, CrossfadeFrames: 0},
			},
		},
		{
			name: "three spans two crossfades",
			rate: 24000, channels: 2, targetPeak: -3,
			sources: []sourceSpec{
				{id: "a", rate: 24000, ch: 1, frames: 12000, freq: 300, amp: 0.4},
				{id: "b", rate: 48000, ch: 1, frames: 12000, freq: 500, amp: 0.3},
				{id: "c", rate: 24000, ch: 2, frames: 9000, freq: 120, amp: 0.6},
			},
			spans: []domain.TimelineSpan{
				{SourceID: "a", SourceStartFrame: 0, SourceEndFrame: 12000, GainDB: 0, CrossfadeFrames: 0},
				{SourceID: "b", SourceStartFrame: 0, SourceEndFrame: 12000, GainDB: -1, CrossfadeFrames: 2000},
				{SourceID: "c", SourceStartFrame: 0, SourceEndFrame: 9000, GainDB: 2, CrossfadeFrames: 1500},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wavs := map[string][]byte{}
			assets := map[string]domain.SourceAsset{}
			loader := memLoader{}
			for _, spec := range tc.sources {
				w := sineWav(t, spec.rate, spec.ch, spec.frames, spec.freq, spec.amp)
				wavs[spec.id] = w
				assets[spec.id] = domain.SourceAsset{ID: spec.id, Digest: spec.id, SampleRate: spec.rate, Channels: spec.ch, FrameCount: int64(spec.frames)}
				loader[spec.id] = SourcePayload{Asset: assets[spec.id], WAV: w}
			}
			plan := domain.Timeline{
				ID: "tl", Name: "t", SampleRate: tc.rate, OutputChannels: tc.channels,
				TargetPeakDBFS: tc.targetPeak, Spans: tc.spans,
			}
			want := referenceRender(t, plan, wavs, assets)

			sink := &captureSink{dir: dir}
			// Force aggressive spilling to exercise the scratch path.
			out, receipt, err := Render(context.Background(), "r", plan, loader, sink,
				Limits{BlockFrames: 4096, ResidentBlocks: 3}, dir, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
			if err != nil {
				t.Fatalf("streaming render: %v", err)
			}
			if !bytes.Equal(sink.written, want) {
				first := -1
				for i := range want {
					if sink.written[i] != want[i] {
						first = i
						break
					}
				}
				// Distinguish header vs data divergence.
				headerDiff := first < 44
				dataSample := (first - 44) / 4
				t.Fatalf("streaming WAV differs: got %d want %d firstDiffByte=%d headerDiff=%v approxFrame=%d",
					len(sink.written), len(want), first, headerDiff, dataSample)
			}
			digest := sha256.Sum256(sink.written)
			if out.Digest != hex.EncodeToString(digest[:]) {
				t.Fatalf("digest mismatch")
			}
			if out.FrameCount != int64(len(want)-44)/int64(2*tc.channels) {
				t.Fatalf("frame count mismatch")
			}
			if receipt.Output.Digest != out.Digest {
				t.Fatalf("receipt digest does not match output")
			}
			// A second render of the same plan must be identical (determinism).
			sink2 := &captureSink{dir: dir}
			out2, _, err := Render(context.Background(), "r", plan, loader, sink2,
				Limits{BlockFrames: 4096, ResidentBlocks: 3}, dir, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
			if err != nil {
				t.Fatalf("second render: %v", err)
			}
			if !bytes.Equal(sink2.written, want) || out2.Digest != out.Digest {
				t.Fatalf("repeated render is not deterministic")
			}
		})
	}
}

type sourceSpec struct {
	id        string
	rate, ch  int
	frames    int
	freq, amp float64
}
