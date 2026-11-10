package service

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"voice-timeline-renderer/internal/domain"
)

func (s *Service) CreateTimeline(ctx context.Context, id string, request TimelineWriteRequest) (domain.Timeline, bool, error) {
	unlock := s.idMu.Lock("timeline:" + id)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return domain.Timeline{}, false, err
	}
	candidate := domain.Timeline{
		ID:             id,
		Name:           strings.TrimSpace(request.Name),
		SampleRate:     request.SampleRate,
		OutputChannels: request.OutputChannels,
		TargetPeakDBFS: request.TargetPeakDBFS,
		Spans:          append([]domain.TimelineSpan(nil), request.Spans...),
		State:          "validating",
	}
	existing, getErr := s.repo.GetTimeline(id)
	if getErr == nil {
		if equivalentTimeline(existing, candidate) {
			return existing, false, nil
		}
		return domain.Timeline{}, false, domain.E(domain.KindConflict, "timeline_conflict", "timeline_id", "identifier is already bound to a different timeline")
	}
	if appErr := domain.AsAppError(getErr); appErr.Kind != domain.KindNotFound {
		return domain.Timeline{}, false, getErr
	}

	sources := make(map[string]domain.SourceAsset)
	for index, span := range candidate.Spans {
		if _, exists := sources[span.SourceID]; exists {
			continue
		}
		asset, err := s.repo.GetSourceMeta(span.SourceID)
		if err != nil {
			if appErr := domain.AsAppError(err); appErr.Kind == domain.KindNotFound {
				return domain.Timeline{}, false, domain.E(
					domain.KindInvalid,
					"source_not_found",
					fmt.Sprintf("spans[%d].source_id", index),
					"referenced source does not exist",
				)
			}
			return domain.Timeline{}, false, err
		}
		sources[span.SourceID] = asset
	}
	if err := domain.ValidateTimeline(candidate, sources, s.config.MaxSpans, s.config.MaxOutputFrames); err != nil {
		return domain.Timeline{}, false, err
	}
	candidate.OutputFrames = domain.TimelineOutputFrames(candidate, sources)
	candidate.State = "ready"
	candidate.CreatedAt = time.Now().UTC()
	if err := s.repo.SaveTimeline(candidate); err != nil {
		return domain.Timeline{}, false, err
	}
	return candidate, true, nil
}

func equivalentTimeline(left, right domain.Timeline) bool {
	if left.Name != right.Name ||
		left.SampleRate != right.SampleRate ||
		left.OutputChannels != right.OutputChannels ||
		math.Abs(left.TargetPeakDBFS-right.TargetPeakDBFS) > 1e-9 ||
		len(left.Spans) != len(right.Spans) {
		return false
	}
	for index := range left.Spans {
		a := left.Spans[index]
		b := right.Spans[index]
		if a.SourceID != b.SourceID ||
			a.SourceStartFrame != b.SourceStartFrame ||
			a.SourceEndFrame != b.SourceEndFrame ||
			math.Abs(a.GainDB-b.GainDB) > 1e-9 ||
			a.CrossfadeFrames != b.CrossfadeFrames {
			return false
		}
	}
	return true
}
