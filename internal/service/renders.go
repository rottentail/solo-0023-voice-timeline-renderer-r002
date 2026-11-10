package service

import (
	"context"
	"time"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

// RetryAfterSeconds hints to clients when a capacity-rejected request can be
// retuned safely.
const RetryAfterSeconds = 2

// CreateRender creates or resumes a render job. Heavy rendering runs on a
// bounded worker pool instead of inside the request lock, so source and
// timeline operations stay responsive. When the pool or memory budget is full
// the durable job remains in queued and the caller receives an explicitly
// retryable error; no output is produced and retrying the same id never
// duplicates artifacts.
func (s *Service) CreateRender(
	ctx context.Context,
	id string,
	request RenderWriteRequest,
) (domain.RenderJob, bool, error) {
	if err := domain.ValidateResourceID("render_id", id); err != nil {
		return domain.RenderJob{}, false, err
	}
	if err := domain.ValidateResourceID("timeline_id", request.TimelineID); err != nil {
		return domain.RenderJob{}, false, err
	}

	// Deduplicate concurrent PUTs for the same render id. Only one pipeline can
	// ever run; waiters observe the single resulting job.
	f, leader := s.inFlight.begin(id)
	if !leader {
		if err := s.inFlight.wait(ctx, f); err != nil {
			return domain.RenderJob{}, false, err
		}
		job, err := s.repo.GetJob(id)
		return job, false, err
	}
	defer s.inFlight.finish(id, f)

	unlock := s.idMu.Lock("render:" + id)
	existing, getErr := s.repo.GetJob(id)
	if getErr == nil {
		unlock()
		if existing.TimelineID != request.TimelineID {
			return existing, false, domain.E(domain.KindConflict, "render_conflict", "render_id", "identifier is already bound to a different timeline")
		}
		return s.resumeExisting(ctx, existing)
	}
	if appErr := domain.AsAppError(getErr); appErr.Kind != domain.KindNotFound {
		unlock()
		return domain.RenderJob{}, false, getErr
	}

	plan, err := s.repo.GetTimeline(request.TimelineID)
	if err != nil {
		unlock()
		return domain.RenderJob{}, false, err
	}
	now := time.Now().UTC()
	job := domain.RenderJob{
		ID:         id,
		TimelineID: plan.ID,
		State:      domain.RenderQueued,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.repo.SaveJob(job); err != nil {
		unlock()
		return domain.RenderJob{}, false, err
	}
	unlock()

	return s.admitAndRun(ctx, job, plan)
}

// resumeExisting handles a repeated PUT for an already-durable job. Completed
// and failed jobs are returned as-is; queued/active jobs are admitted again so
// a capacity rejection can be retried safely.
func (s *Service) resumeExisting(ctx context.Context, job domain.RenderJob) (domain.RenderJob, bool, error) {
	switch job.State {
	case domain.RenderCompleted, domain.RenderFailed:
		if job.State == domain.RenderFailed {
			return job, false, domain.AsAppError(jobError(job))
		}
		return job, false, nil
	default:
		plan, err := s.repo.GetTimeline(job.TimelineID)
		if err != nil {
			return job, false, err
		}
		return s.admitAndRun(ctx, job, plan)
	}
}

// admitAndRun attempts to reserve a worker slot and memory. On success it runs
// the pipeline; on capacity saturation it leaves the job queued and returns a
// stable retryable error.
func (s *Service) admitAndRun(ctx context.Context, job domain.RenderJob, plan domain.Timeline) (domain.RenderJob, bool, error) {
	estimate, err := s.estimateBytes(plan)
	if err != nil {
		return s.failRender(job, err)
	}
	// A single job larger than the whole budget can never succeed: fail it
	// deterministically rather than returning endless retryable pressure.
	if estimate > s.config.RenderMemoryBytes {
		return s.failRender(job, domain.E(
			domain.KindTooLarge,
			"render_memory_budget_exceeded",
			"render_id",
			"render requires more memory than the configured budget",
		))
	}

	select {
	case s.renderSlots <- struct{}{}:
	default:
		return job, true, s.capacityError(job)
	}
	s.admitMu.Lock()
	if s.reserved+estimate > s.config.RenderMemoryBytes {
		s.admitMu.Unlock()
		<-s.renderSlots
		return job, true, s.capacityError(job)
	}
	s.reserved += estimate
	s.admitMu.Unlock()

	release := func() {
		s.admitMu.Lock()
		s.reserved -= estimate
		s.admitMu.Unlock()
		<-s.renderSlots
	}
	defer release()

	// The pipeline runs detached from the HTTP request: a client disconnect or
	// timeout must not corrupt a durable, single-use job.
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	return s.execute(runCtx, job, plan)
}

func (s *Service) capacityError(job domain.RenderJob) error {
	return domain.E(
		domain.KindUnavailable,
		"render_capacity_exhausted",
		"render_id",
		"render capacity is saturated; retry the same render_id after the indicated interval",
	)
}

// reconcilePublished adopts an already-published output folder left behind by a
// crash between artifact commit and the completed state transition. It returns
// ok=false when no artifacts exist, so the normal pipeline runs.
func (s *Service) reconcilePublished(job domain.RenderJob) (domain.RenderJob, bool, error) {
	_, receipt, err := s.repo.GetRenderOutput(job.ID)
	if err != nil {
		if appErr := domain.AsAppError(err); appErr.Kind == domain.KindUnready || appErr.Kind == domain.KindNotFound {
			return domain.RenderJob{}, false, nil
		}
		return domain.RenderJob{}, false, err
	}
	job.Output = &receipt.Output
	job.ReceiptDigest = receipt.Output.Digest
	// Move through the recorded state machine where possible, then complete.
	if job.State == domain.RenderQueued {
		if err := s.advanceJob(&job, domain.RenderRendering, nil); err != nil {
			return job, true, err
		}
	}
	if job.State == domain.RenderRendering {
		if err := s.advanceJob(&job, domain.RenderFinalizing, nil); err != nil {
			return job, true, err
		}
	}
	if err := s.advanceJob(&job, domain.RenderCompleted, nil); err != nil {
		return job, true, err
	}
	return job, true, nil
}

// execute drives one deterministic pipeline through the durable state machine.
// All rendering is streaming and block-spilled, so memory stays within budget.
func (s *Service) execute(ctx context.Context, job domain.RenderJob, plan domain.Timeline) (domain.RenderJob, bool, error) {
	// Crash reconciliation: a previous process may have published the output
	// folder but died before recording the completed transition. Adopt the
	// existing artifacts instead of re-rendering, which would conflict on the
	// already-published folder and could never produce a second receipt.
	if reconciled, ok, err := s.reconcilePublished(job); err != nil {
		return job, true, err
	} else if ok {
		return reconciled, true, nil
	}

	if job.State == domain.RenderQueued {
		if err := s.advanceJob(&job, domain.RenderRendering, nil); err != nil {
			return job, true, err
		}
	} else if err := job.ReAdopt(time.Now().UTC()); err != nil {
		return job, true, err
	} else if err := s.repo.SaveJob(job); err != nil {
		return job, true, err
	}

	loader := &serviceSourceLoader{repo: s.repo}
	sink := s.repo.NewRenderSink(job.ID)
	output, receipt, renderErr := render.Render(
		ctx,
		job.ID,
		plan,
		loader,
		sink,
		render.Limits{
			MaxOutputFrames: s.config.MaxOutputFrames,
			BlockFrames:     s.config.BlockFrames,
			ResidentBlocks:  s.config.ResidentBlocks,
		},
		s.repo.ScratchDir(),
		time.Now().UTC(),
	)
	if renderErr != nil {
		return s.failRender(job, domain.Wrap(domain.KindInternal, "render_failed", "", "timeline rendering failed", renderErr))
	}
	if err := s.advanceJob(&job, domain.RenderFinalizing, nil); err != nil {
		return job, true, err
	}
	job.Output = &output
	job.ReceiptDigest = output.Digest
	if err := s.advanceJob(&job, domain.RenderCompleted, nil); err != nil {
		return job, true, err
	}
	_ = receipt
	return job, true, nil
}

// estimateBytes computes the conservative per-render admission footprint from
// stored source metadata, without decoding any media.
func (s *Service) estimateBytes(plan domain.Timeline) (int64, error) {
	unique := make(map[string]struct{})
	var sourceBytes int64
	for _, span := range plan.Spans {
		if _, ok := unique[span.SourceID]; ok {
			continue
		}
		unique[span.SourceID] = struct{}{}
		asset, err := s.repo.GetSourceMeta(span.SourceID)
		if err != nil {
			return 0, err
		}
		sourceBytes += asset.ByteCount
	}
	limits := render.Limits{
		BlockFrames:    s.config.BlockFrames,
		ResidentBlocks: s.config.ResidentBlocks,
	}
	return render.EstimateRenderBytes(sourceBytes, plan.OutputChannels, limits), nil
}

func (s *Service) advanceJob(job *domain.RenderJob, state domain.RenderState, detail *domain.ErrorDetail) error {
	if err := job.Transition(state, detail, time.Now().UTC()); err != nil {
		return err
	}
	if err := s.repo.SaveJob(*job); err != nil {
		return err
	}
	return nil
}

func (s *Service) failRender(job domain.RenderJob, cause error) (domain.RenderJob, bool, error) {
	appErr := domain.AsAppError(cause)
	detail := &domain.ErrorDetail{
		Code:    appErr.Detail.Code,
		Message: appErr.Detail.Message,
		Field:   appErr.Detail.Field,
	}
	transitionErr := job.Transition(domain.RenderFailed, detail, time.Now().UTC())
	if transitionErr != nil {
		return job, true, transitionErr
	}
	job.Output = nil
	job.ReceiptDigest = ""
	if saveErr := s.repo.SaveJob(job); saveErr != nil {
		return job, true, saveErr
	}
	return job, true, appErr
}

// jobError reconstructs an AppError from a terminal failed job's stored detail.
func jobError(job domain.RenderJob) error {
	if job.Error == nil {
		return domain.E(domain.KindInternal, "render_failed", "", "render job failed")
	}
	return domain.E(domain.KindInternal, job.Error.Code, job.Error.Field, job.Error.Message)
}

// serviceSourceLoader resolves sources through the repository on demand. The
// render-side cache decides how many decoded clips stay resident.
type serviceSourceLoader struct {
	repo Repository
}

func (loader *serviceSourceLoader) Load(ctx context.Context, sourceID string) (render.SourcePayload, error) {
	if err := ctx.Err(); err != nil {
		return render.SourcePayload{}, err
	}
	asset, wav, err := loader.repo.GetSource(sourceID)
	if err != nil {
		return render.SourcePayload{}, err
	}
	return render.SourcePayload{Asset: asset, WAV: wav}, nil
}
