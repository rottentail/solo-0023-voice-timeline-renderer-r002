package service

import (
	"context"
	"runtime/debug"
	"sync"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

type Repository interface {
	SaveSource(domain.SourceAsset, []byte) error
	GetSourceMeta(string) (domain.SourceAsset, error)
	GetSource(string) (domain.SourceAsset, []byte, error)
	SaveTimeline(domain.Timeline) error
	GetTimeline(string) (domain.Timeline, error)
	SaveJob(domain.RenderJob) error
	GetJob(string) (domain.RenderJob, error)
	ListJobIDs() ([]string, error)
	NewRenderSink(renderID string) render.Sink
	ScratchDir() string
	GetRenderOutput(string) ([]byte, domain.RenderReceipt, error)
}

// Config controls the bounded render path. The defaults guarantee two parallel
// renders inside an explicit memory budget while keeping short operations
// independent of long-running renders.
type Config struct {
	MaxSourceBytes     int64
	MaxSpans           int
	MaxOutputFrames    int64
	MaxParallelRenders int
	RenderMemoryBytes  int64
	BlockFrames        int64
	ResidentBlocks     int
}

// DefaultConfig raises the output cap to thirty minutes at 48 kHz and reserves
// a one-gibibyte render memory budget shared by up to two concurrent renders.
func DefaultConfig() Config {
	return Config{
		MaxSourceBytes:     32 << 20,
		MaxSpans:           64,
		MaxOutputFrames:    48000 * 1800,
		MaxParallelRenders: 2,
		RenderMemoryBytes:  1 << 30,
		BlockFrames:        render.DefaultBlockFrames,
		ResidentBlocks:     render.DefaultResidentBlocks,
	}
}

type Service struct {
	repo   Repository
	config Config

	// idMu serializes create-or-get decisions per resource identifier while
	// never spanning the heavy render work.
	idMu keyedMutex

	// render admission: a counting semaphore plus a live reservation total.
	renderSlots chan struct{}
	admitMu     sync.Mutex
	reserved    int64

	// inFlight dedupes concurrent PUTs for the same render id so a retried
	// request cannot start a second pipeline or publish twice.
	inFlight flightMap
}

func New(repo Repository, config Config) *Service {
	defaults := DefaultConfig()
	if config.MaxSourceBytes <= 0 {
		config.MaxSourceBytes = defaults.MaxSourceBytes
	}
	if config.MaxSpans <= 0 {
		config.MaxSpans = defaults.MaxSpans
	}
	if config.MaxOutputFrames <= 0 {
		config.MaxOutputFrames = defaults.MaxOutputFrames
	}
	if config.MaxParallelRenders <= 0 {
		config.MaxParallelRenders = defaults.MaxParallelRenders
	}
	if config.RenderMemoryBytes <= 0 {
		config.RenderMemoryBytes = defaults.RenderMemoryBytes
	}
	if config.BlockFrames <= 0 {
		config.BlockFrames = defaults.BlockFrames
	}
	if config.ResidentBlocks <= 0 {
		config.ResidentBlocks = defaults.ResidentBlocks
	}
	svc := &Service{
		repo:        repo,
		config:      config,
		renderSlots: make(chan struct{}, config.MaxParallelRenders),
	}
	// Keep the Go runtime from exceeding the explicit render budget under
	// transient garbage; the block store itself guarantees a hard cap.
	debug.SetMemoryLimit(config.RenderMemoryBytes + (256 << 20))
	debug.SetGCPercent(50)
	return svc
}

type SourceWriteRequest struct {
	Name      string `json:"name"`
	WAVBase64 string `json:"wav_base64"`
}

type TimelineWriteRequest struct {
	Name           string                `json:"name"`
	SampleRate     int                   `json:"sample_rate"`
	OutputChannels int                   `json:"output_channels"`
	TargetPeakDBFS float64               `json:"target_peak_dbfs"`
	Spans          []domain.TimelineSpan `json:"spans"`
}

type RenderWriteRequest struct {
	TimelineID string `json:"timeline_id"`
}

func (s *Service) GetSource(ctx context.Context, id string) (domain.SourceAsset, error) {
	if err := ctx.Err(); err != nil {
		return domain.SourceAsset{}, err
	}
	return s.repo.GetSourceMeta(id)
}

func (s *Service) GetTimeline(ctx context.Context, id string) (domain.Timeline, error) {
	if err := ctx.Err(); err != nil {
		return domain.Timeline{}, err
	}
	return s.repo.GetTimeline(id)
}

func (s *Service) GetRender(ctx context.Context, id string) (domain.RenderJob, error) {
	if err := ctx.Err(); err != nil {
		return domain.RenderJob{}, err
	}
	return s.repo.GetJob(id)
}

func (s *Service) GetReceipt(ctx context.Context, id string) (domain.RenderReceipt, error) {
	job, err := s.GetRender(ctx, id)
	if err != nil {
		return domain.RenderReceipt{}, err
	}
	if job.State != domain.RenderCompleted {
		return domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render receipt is available only after completion")
	}
	_, receipt, err := s.repo.GetRenderOutput(id)
	return receipt, err
}

func (s *Service) GetOutput(ctx context.Context, id string) ([]byte, domain.RenderReceipt, error) {
	job, err := s.GetRender(ctx, id)
	if err != nil {
		return nil, domain.RenderReceipt{}, err
	}
	if job.State != domain.RenderCompleted {
		return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is available only after completion")
	}
	return s.repo.GetRenderOutput(id)
}
