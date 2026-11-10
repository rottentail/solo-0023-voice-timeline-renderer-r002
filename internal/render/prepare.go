package render

import (
	"context"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// preparedSpan records placement, gain metadata, and the transformed clip the
// mixer consumes. The clip is released immediately after the span has been
// accumulated into every output block it touches, so peak memory stays bounded
// by a few spans rather than the whole timeline.
type preparedSpan struct {
	Index            int
	SourceID         string
	SourceDigest     string
	SourceStartFrame int64
	SourceEndFrame   int64
	OutputStartFrame int64
	OutputEndFrame   int64
	OutputFrames     int64
	CrossfadeFrames  int64
	GainDB           float64
	ChannelAction    string

	clip *audiocodec.Clip
}

// release drops the span's decoded samples after it has been mixed.
func (span *preparedSpan) release() {
	span.clip = nil
}

// loaderCache keeps decoded source payloads and clips bounded. At most two
// distinct sources are held resident: spans typically touch one or two
// neighboring sources, and evicting older entries bounds memory even for
// timelines that reference many large sources. Repeated references to the same
// source reuse the cached clip instead of decoding the WAV again.
type loaderCache struct {
	loader  SourceLoader
	payload map[string]SourcePayload
	clips   map[string]*audiocodec.Clip
	order   []string
}

func newLoaderCache(loader SourceLoader) *loaderCache {
	return &loaderCache{
		loader:  loader,
		payload: make(map[string]SourcePayload),
		clips:   make(map[string]*audiocodec.Clip),
	}
}

func (cache *loaderCache) Load(ctx context.Context, sourceID string) (SourcePayload, error) {
	if payload, ok := cache.payload[sourceID]; ok {
		return payload, nil
	}
	payload, err := cache.loader.Load(ctx, sourceID)
	if err != nil {
		return SourcePayload{}, err
	}
	cache.payload[sourceID] = payload
	cache.order = append(cache.order, sourceID)
	cache.evict()
	return payload, nil
}

// clip decodes the source WAV once and reuses the result for every span that
// references the same source.
func (cache *loaderCache) clip(ctx context.Context, sourceID string) (*audiocodec.Clip, error) {
	if clip, ok := cache.clips[sourceID]; ok {
		return clip, nil
	}
	payload, err := cache.Load(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	clip, _, err := audiocodec.DecodeWAV(payload.WAV)
	if err != nil {
		return nil, err
	}
	cache.clips[sourceID] = clip
	cache.evict()
	return clip, nil
}

func (cache *loaderCache) evict() {
	const maxResident = 2
	for len(cache.order) > maxResident {
		evicted := cache.order[0]
		cache.order = cache.order[1:]
		delete(cache.payload, evicted)
		delete(cache.clips, evicted)
	}
}

// prepareSpans validates every span, decodes and transforms its source
// interval, and computes output placement.
func prepareSpans(
	ctx context.Context,
	plan domain.Timeline,
	loader SourceLoader,
) ([]preparedSpan, int64, error) {
	cache := newLoaderCache(loader)
	prepared := make([]preparedSpan, 0, len(plan.Spans))
	cursor := int64(0)
	for index, span := range plan.Spans {
		payload, err := cache.Load(ctx, span.SourceID)
		if err != nil {
			return nil, 0, err
		}
		clip, err := cache.clip(ctx, span.SourceID)
		if err != nil {
			return nil, 0, err
		}
		selected, err := audiocodec.SliceFrames(ctx, clip, span.SourceStartFrame, span.SourceEndFrame)
		if err != nil {
			return nil, 0, err
		}
		resampled, err := audiocodec.Resample(ctx, selected, plan.SampleRate)
		if err != nil {
			return nil, 0, err
		}
		mapped, action, err := audiocodec.MapChannels(ctx, resampled, plan.OutputChannels)
		if err != nil {
			return nil, 0, err
		}
		frames := mapped.FrameCount()
		if frames < 1 {
			return nil, 0, errEmptySpan
		}

		start := cursor
		if index > 0 {
			start -= span.CrossfadeFrames
		}
		if start < 0 {
			return nil, 0, errNegativePosition
		}
		end := start + frames
		cursor = end

		prepared = append(prepared, preparedSpan{
			Index:            index,
			SourceID:         span.SourceID,
			SourceDigest:     payload.Asset.Digest,
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			OutputStartFrame: start,
			OutputEndFrame:   end,
			OutputFrames:     frames,
			CrossfadeFrames:  span.CrossfadeFrames,
			GainDB:           span.GainDB,
			ChannelAction:    action,
			clip:             mapped,
		})
	}
	return prepared, cursor, nil
}
