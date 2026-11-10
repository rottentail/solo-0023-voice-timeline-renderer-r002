package render

import (
	"context"
	"errors"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// Limits bounds a single render. MaxOutputFrames rejects over-long timelines
// before any allocation-heavy work.
type Limits struct {
	MaxOutputFrames int64
	BlockFrames     int64
	ResidentBlocks  int
}

// SourceLoader resolves a source on demand. Implementations may cache decoded
// clips, but the renderer never holds every source at once, so peak memory is
// bounded by the loader's own cache policy.
type SourceLoader interface {
	Load(ctx context.Context, sourceID string) (SourcePayload, error)
}

// SourcePayload is the minimum information required to evaluate one span.
type SourcePayload struct {
	Asset domain.SourceAsset
	WAV   []byte
}

// Sink receives the finished artifacts. Render remains storage-agnostic: the
// sink owns temp-file staging and atomic publication.
type Sink interface {
	// NewOutput opens the destination WAV for streaming. The writer hashes
	// every byte (header included) and stages output away from the published
	// location.
	NewOutput(sampleRate, channels int) (*FileOutput, error)
	// Commit publishes the WAV already written through the output together
	// with its receipt, atomically.
	Commit(output *FileOutput, receipt domain.RenderReceipt) error
	// Abort discards any staged output after a failure.
	Abort(output *FileOutput)
}

// Render runs the full deterministic pipeline against a bounded memory budget.
// The numeric operations are identical to the original in-memory renderer, so
// the same timeline always produces byte-identical output.
func Render(
	ctx context.Context,
	renderID string,
	plan domain.Timeline,
	loader SourceLoader,
	sink Sink,
	limits Limits,
	scratchDir string,
	now time.Time,
) (domain.OutputAsset, domain.RenderReceipt, error) {
	spans, totalFrames, err := prepareSpans(ctx, plan, loader)
	if err != nil {
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	if totalFrames < 1 {
		return domain.OutputAsset{}, domain.RenderReceipt{}, errors.New("timeline produced no output frames")
	}
	if limits.MaxOutputFrames > 0 && totalFrames > limits.MaxOutputFrames {
		return domain.OutputAsset{}, domain.RenderReceipt{}, errors.New("rendered timeline exceeds the configured frame limit")
	}

	scratch, err := openScratch(scratchDir)
	if err != nil {
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	defer func() {
		name := scratch.Name()
		_ = scratch.Close()
		_ = removeScratch(name)
	}()
	blockFrames, residentBlocks := normalizeBlockLimits(limits)
	store := newBlockStore(scratch, plan.OutputChannels, blockFrames, residentBlocks)

	before, peakLinear, err := mixPrepared(ctx, spans, totalFrames, plan.OutputChannels, store)
	if err != nil {
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	normalization := planNormalization(before, peakLinear, plan.TargetPeakDBFS)

	writer, err := sink.NewOutput(plan.SampleRate, plan.OutputChannels)
	if err != nil {
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	output, outputMetrics, err := encodeNormalized(
		ctx,
		store,
		writer,
		plan.SampleRate,
		plan.OutputChannels,
		totalFrames,
		normalization,
	)
	if err != nil {
		sink.Abort(writer)
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	if err := writer.Close(); err != nil {
		sink.Abort(writer)
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	output.Digest = writer.DigestHex()
	output.ByteCount = writer.bytesWritten()

	receipt := buildReceipt(renderID, plan, output, outputMetrics, normalization, spans, now)
	if err := sink.Commit(writer, receipt); err != nil {
		sink.Abort(writer)
		return domain.OutputAsset{}, domain.RenderReceipt{}, err
	}
	return output, receipt, nil
}

func normalizeBlockLimits(limits Limits) (int64, int) {
	blockFrames := limits.BlockFrames
	if blockFrames <= 0 {
		blockFrames = DefaultBlockFrames
	}
	resident := limits.ResidentBlocks
	if resident < 2 {
		resident = DefaultResidentBlocks
	}
	return blockFrames, resident
}

// EstimateRenderBytes is a conservative upper-bound admission estimate covering
// the resident mix blocks, the largest in-flight source/span copies, and runtime
// overhead. It deliberately overestimates so the hard block-store cap is never
// the first limit hit.
func EstimateRenderBytes(uniqueSourceBytes int64, channels int, limits Limits) int64 {
	blockFrames, residentBlocks := normalizeBlockLimits(limits)
	// Resident mix blocks hold float32 accumulators (4 bytes per sample).
	residentMix := blockFrames * int64(channels) * 4 * int64(residentBlocks)
	// Worst transient while transforming one source: decoded float copy plus
	// resampled and channel-mapped copies.
	sourceWorking := uniqueSourceBytes * 2 * 4
	if sourceWorking < residentMix {
		sourceWorking = residentMix
	}
	overhead := int64(96 << 20)
	return residentMix + sourceWorking + overhead
}

// config defaults used when callers do not supply explicit block limits.
const (
	DefaultBlockFrames    int64 = 32768
	DefaultResidentBlocks       = 64
)
