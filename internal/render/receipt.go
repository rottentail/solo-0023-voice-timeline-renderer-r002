package render

import (
	"fmt"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// buildReceipt constructs the immutable render record from the streaming
// results. The output metrics are measured over the quantized PCM16 samples
// during encoding, which is exactly the values the original implementation
// obtained by decoding the finished WAV.
func buildReceipt(
	renderID string,
	plan domain.Timeline,
	output domain.OutputAsset,
	outputMetrics audiocodec.Metrics,
	normalized normalizationResult,
	spans []preparedSpan,
	now time.Time,
) domain.RenderReceipt {
	receiptSpans := make([]domain.ReceiptSpan, 0, len(spans))
	warnings := make([]string, 0)
	for _, span := range spans {
		receiptSpans = append(receiptSpans, domain.ReceiptSpan{
			Index:            span.Index,
			SourceID:         span.SourceID,
			SourceDigest:     span.SourceDigest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: span.OutputStartFrame,
			OutputEndFrame:   span.OutputEndFrame,
			OutputFrames:     span.OutputFrames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    span.ChannelAction,
		})
		if span.ChannelAction != "unchanged" {
			warnings = append(warnings, fmt.Sprintf("span %d applied %s", span.Index, span.ChannelAction))
		}
	}
	receipt := domain.RenderReceipt{
		SchemaVersion: 1,
		RenderID:      renderID,
		TimelineID:    plan.ID,
		Output:        output,
		Normalization: domain.NormalizationReceipt{
			RequestedPeakDBFS: plan.TargetPeakDBFS,
			BeforePeakDBFS:    normalized.Before.PeakDBFS,
			AppliedGainDB:     normalized.GainDB,
			OutputPeakDBFS:    outputMetrics.PeakDBFS,
		},
		Spans:     receiptSpans,
		Warnings:  warnings,
		CreatedAt: now.UTC(),
	}
	return receipt
}

func framesToMilliseconds(frames int64, sampleRate int) int64 {
	if frames <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(float64(frames) * 1000 / float64(sampleRate))
}
