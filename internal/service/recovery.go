package service

import (
	"context"
	"log/slog"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// RecoverInterruptedJobs adopts render jobs left in a non-terminal state by a
// previous process. Each is re-run through the deterministic pipeline using
// the same bounded admission path, so a crash never leaves a job stuck in
// queued/rendering/finalizing and never double-publishes output.
func (s *Service) RecoverInterruptedJobs(ctx context.Context) {
	ids, err := s.repo.ListJobIDs()
	if err != nil {
		slog.Error("cannot list render jobs during recovery", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		job, err := s.repo.GetJob(id)
		if err != nil {
			continue
		}
		if job.State == domain.RenderCompleted || job.State == domain.RenderFailed {
			continue
		}
		plan, err := s.repo.GetTimeline(job.TimelineID)
		if err != nil {
			slog.Error("recovery cannot resolve timeline; failing job", "render_id", id, "error", err)
			if _, _, failErr := s.failRender(job, domain.Wrap(domain.KindInternal, "render_failed", "", "timeline cannot be resolved during recovery", err)); failErr != nil {
				slog.Error("recovery failed to mark job failed", "render_id", id, "error", failErr)
			}
			continue
		}
		s.recoverOne(ctx, job, plan)
	}
}

func (s *Service) recoverOne(ctx context.Context, job domain.RenderJob, plan domain.Timeline) {
	// Recovery is best-effort and must not block startup indefinitely. If
	// capacity stays saturated, the job remains queued and can be retried via
	// PUT/GET like any other capacity-rejected request.
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		terminal, result, err := s.tryRecover(ctx, job, plan)
		if terminal {
			if err != nil {
				slog.Error("interrupted render recovered as failed", "render_id", job.ID, "error", err)
			} else {
				slog.Info("interrupted render recovered", "render_id", job.ID, "state", result.State)
			}
			return
		}
		select {
		case <-deadline.Done():
			slog.Warn("interrupted render still queued after recovery window", "render_id", job.ID)
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) tryRecover(ctx context.Context, job domain.RenderJob, plan domain.Timeline) (bool, domain.RenderJob, error) {
	f, leader := s.inFlight.begin(job.ID)
	if !leader {
		_ = s.inFlight.wait(ctx, f)
		latest, err := s.repo.GetJob(job.ID)
		return true, latest, err
	}
	defer s.inFlight.finish(job.ID, f)

	// Re-read state: another request may have handled it while we waited.
	latest, err := s.repo.GetJob(job.ID)
	if err == nil && (latest.State == domain.RenderCompleted || latest.State == domain.RenderFailed) {
		return true, latest, nil
	}
	result, _, runErr := s.admitAndRun(ctx, job, plan)
	if runErr != nil {
		appErr := domain.AsAppError(runErr)
		if appErr.Detail.Code == "render_capacity_exhausted" {
			return false, result, nil
		}
		return true, result, runErr
	}
	return true, result, nil
}
