package service

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

func (s *Service) CreateSource(ctx context.Context, id string, request SourceWriteRequest) (domain.SourceAsset, bool, error) {
	unlock := s.idMu.Lock("source:" + id)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return domain.SourceAsset{}, false, err
	}
	if err := domain.ValidateResourceID("source_id", id); err != nil {
		return domain.SourceAsset{}, false, err
	}
	request.Name = strings.TrimSpace(request.Name)
	if err := domain.ValidateName(request.Name); err != nil {
		return domain.SourceAsset{}, false, err
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(request.WAVBase64)
	if err != nil {
		return domain.SourceAsset{}, false, domain.E(domain.KindInvalid, "invalid_base64", "wav_base64", "media payload must be valid base64")
	}
	if int64(len(payload)) > s.config.MaxSourceBytes {
		return domain.SourceAsset{}, false, domain.E(domain.KindTooLarge, "source_too_large", "wav_base64", "media payload exceeds the configured byte limit")
	}
	digestBytes := sha256.Sum256(payload)
	digest := hex.EncodeToString(digestBytes[:])

	existing, getErr := s.repo.GetSourceMeta(id)
	if getErr == nil {
		if existing.Digest != digest || existing.Name != request.Name {
			return domain.SourceAsset{}, false, domain.E(domain.KindConflict, "source_conflict", "source_id", "identifier is already bound to different source content")
		}
		return existing, false, nil
	} else if appErr := domain.AsAppError(getErr); appErr.Kind != domain.KindNotFound {
		return domain.SourceAsset{}, false, getErr
	}

	clip, info, decodeErr := audiocodec.DecodeWAV(payload)
	if decodeErr != nil {
		return domain.SourceAsset{}, false, domain.Wrap(
			domain.KindInvalid,
			"invalid_wav",
			"wav_base64",
			"payload must be a RIFF/WAVE file containing 16-bit integer PCM with one or two channels",
			decodeErr,
		)
	}
	facts := domain.SourceFacts{
		SampleRate:    info.SampleRate,
		Channels:      info.Channels,
		BitsPerSample: info.BitsPerSample,
		FrameCount:    info.FrameCount,
	}
	if err := domain.ValidateSourceFacts("source", facts); err != nil {
		return domain.SourceAsset{}, false, err
	}
	measurements := audiocodec.Measure(clip.Samples)
	asset := domain.SourceAsset{
		ID:            id,
		Name:          request.Name,
		Digest:        digest,
		ByteCount:     info.ByteCount,
		SampleRate:    info.SampleRate,
		Channels:      info.Channels,
		BitsPerSample: info.BitsPerSample,
		FrameCount:    info.FrameCount,
		DurationMS:    durationMilliseconds(info.FrameCount, info.SampleRate),
		PeakDBFS:      measurements.PeakDBFS,
		RMSDBFS:       measurements.RMSDBFS,
		CreatedAt:     time.Now().UTC(),
	}
	if err := s.repo.SaveSource(asset, payload); err != nil {
		return domain.SourceAsset{}, false, err
	}
	return asset, true, nil
}

func durationMilliseconds(frames int64, sampleRate int) int64 {
	if frames <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(float64(frames) * 1000 / float64(sampleRate))
}
