package render

import (
	"context"
	"io"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

// mixPrepared accumulates every span into the block store in the original span
// order. The per-frame arithmetic — outgoing crossfade weight applied before
// the incoming sample, then the add — is performed in float32 exactly as in
// the in-memory mixer, so results are bit-identical. Blocks in a crossfade
// region are pinned for the duration of the two spans that overlap them, which
// guarantees such a block is never spilled and reloaded before all its
// contributions are applied; eviction therefore cannot change rounding.
func mixPrepared(
	ctx context.Context,
	spans []preparedSpan,
	totalFrames int64,
	channels int,
	store *blockStore,
) (audiocodec.Metrics, float64, error) {
	if totalFrames < 1 || channels < 1 {
		return audiocodec.Metrics{}, 0, errInvalidMix
	}
	if totalFrames > int64(^uint(0)>>1)/int64(channels) {
		return audiocodec.Metrics{}, 0, errMixTooLarge
	}
	store.SetFrameCount(totalFrames)

	for spanIndex := range spans {
		span := &spans[spanIndex]
		// A crossfade means this span rewrites samples produced by its
		// predecessor. Pin the overlapping output range until this span has
		// been applied so those blocks stay resident throughout.
		pinned := false
		if span.CrossfadeFrames > 0 {
			overlapStart := span.OutputStartFrame
			overlapEnd := span.OutputStartFrame + span.CrossfadeFrames
			store.PinRange(overlapStart, overlapEnd)
			pinned = true
		}
		gain := float32(audiocodec.DBToLinear(span.GainDB))
		frames := span.clip.FrameCount()
		for frame := int64(0); frame < frames; frame++ {
			if frame%4096 == 0 {
				select {
				case <-ctx.Done():
					if pinned {
						store.UnpinRange(span.OutputStartFrame, span.OutputStartFrame+span.CrossfadeFrames)
					}
					return audiocodec.Metrics{}, 0, ctx.Err()
				default:
				}
			}
			incoming := float32(1)
			outgoing := float32(1)
			if span.CrossfadeFrames > 0 && frame < span.CrossfadeFrames {
				progress := (float64(frame) + 0.5) / float64(span.CrossfadeFrames)
				angle := progress * math.Pi / 2
				incoming = float32(math.Sin(angle))
				outgoing = float32(math.Cos(angle))
			}
			inStart := int(frame) * channels
			outFrame := span.OutputStartFrame + frame
			for channel := 0; channel < channels; channel++ {
				value := span.clip.Samples[inStart+channel] * gain * incoming
				if span.CrossfadeFrames > 0 && frame < span.CrossfadeFrames {
					store.scaleSample(outFrame, int64(channel), outgoing)
				}
				store.addSample(outFrame, int64(channel), value)
			}
		}
		if pinned {
			store.UnpinRange(span.OutputStartFrame, span.OutputStartFrame+span.CrossfadeFrames)
		}
		span.release()
	}
	return measureStore(ctx, store, channels)
}

// measureStore scans every block once and accumulates peak and RMS exactly like
// audiocodec.Measure over the full contiguous buffer. The raw linear peak is
// returned so normalization can use the same unrounded peak the original
// in-memory pipeline consumed.
func measureStore(ctx context.Context, store *blockStore, channels int) (audiocodec.Metrics, float64, error) {
	acc := &audiocodec.MetricsAccumulator{}
	reader := store.SequentialReader()
	scanned := int64(0)
	for {
		if scanned%int64(4096*64) == 0 {
			select {
			case <-ctx.Done():
				return audiocodec.Metrics{}, 0, ctx.Err()
			default:
			}
		}
		block, validFrames, err := reader.NextBlock()
		if err == io.EOF {
			break
		}
		if err != nil {
			return audiocodec.Metrics{}, 0, err
		}
		acc.Add(block[:validFrames*channels])
		scanned += int64(validFrames)
	}
	return acc.Metrics(), acc.PeakLinear(), nil
}
