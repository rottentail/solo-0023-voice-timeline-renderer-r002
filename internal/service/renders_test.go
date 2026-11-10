package service

import (
	"context"
	"encoding/base64"
	"math"
	"sync"
	"testing"
	"time"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/storage"
)

func makeWAV(t *testing.T, sampleRate, channels, frames int, freq, amp float64) []byte {
	t.Helper()
	samples := make([]float32, frames*channels)
	for f := 0; f < frames; f++ {
		seconds := float64(f) / float64(sampleRate)
		first := float32(math.Sin(2*math.Pi*freq*seconds) * amp)
		if channels == 1 {
			samples[f] = first
			continue
		}
		samples[f*2] = first
		samples[f*2+1] = first * 0.8
	}
	wav, err := audiocodec.EncodePCM16(sampleRate, channels, samples)
	if err != nil {
		t.Fatal(err)
	}
	return wav
}

func newTestService(t *testing.T, config Config) (*Service, *storage.Repository) {
	t.Helper()
	repo, err := storage.NewRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(repo, config), repo
}

func seedTimeline(t *testing.T, svc *Service, tlID string, frames int) domain.Timeline {
	t.Helper()
	ctx := context.Background()
	wav := makeWAV(t, 48000, 1, frames, 220, 0.3)
	if _, _, err := svc.CreateSource(ctx, "src_seed_01", SourceWriteRequest{Name: "s", WAVBase64: base64.StdEncoding.EncodeToString(wav)}); err != nil {
		t.Fatal(err)
	}
	plan, _, err := svc.CreateTimeline(ctx, tlID, TimelineWriteRequest{
		Name: "t", SampleRate: 48000, OutputChannels: 2, TargetPeakDBFS: -1,
		Spans: []domain.TimelineSpan{{SourceID: "src_seed_01", SourceStartFrame: 0, SourceEndFrame: int64(frames), GainDB: 0, CrossfadeFrames: 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestRenderSucceeds(t *testing.T) {
	svc, _ := newTestService(t, DefaultConfig())
	plan := seedTimeline(t, svc, "tl_ok_001", 12000)
	job, created, err := svc.CreateRender(context.Background(), "render_ok_01", RenderWriteRequest{TimelineID: plan.ID})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !created || job.State != domain.RenderCompleted || job.Output == nil {
		t.Fatalf("unexpected job created=%v state=%s output=%v", created, job.State, job.Output)
	}
}

func TestCapacityRejectionIsRetryable(t *testing.T) {
	config := DefaultConfig()
	config.MaxParallelRenders = 1
	config.RenderMemoryBytes = 1 << 30
	svc, _ := newTestService(t, config)

	// Two distinct renders; with a single slot the second must be rejected as
	// retryable and remain queued (no artifacts, no failure).
	planA := seedTimeline(t, svc, "tl_cap_a_01", 24000)
	planB := seedTimeline(t, svc, "tl_cap_b_01", 24000)

	// Hold the only slot directly to force saturation deterministically.
	svc.renderSlots <- struct{}{}

	jobB, created, err := svc.CreateRender(context.Background(), "render_cap_b_01", RenderWriteRequest{TimelineID: planB.ID})
	if err == nil {
		t.Fatal("expected capacity error")
	}
	appErr := domain.AsAppError(err)
	if appErr.Kind != domain.KindUnavailable || appErr.Detail.Code != "render_capacity_exhausted" {
		t.Fatalf("expected retryable capacity error, got %s/%s", appErr.Kind, appErr.Detail.Code)
	}
	if jobB.State != domain.RenderQueued {
		t.Fatalf("rejected job must stay queued, got %s", jobB.State)
	}
	if !created {
		t.Fatal("queued job should be reported as a created resource")
	}
	// No output or receipt must be observable for a queued job.
	if _, _, err := svc.GetOutput(context.Background(), "render_cap_b_01"); err == nil {
		t.Fatal("queued render must not expose output")
	}

	// Releasing the slot and retrying the same id must complete exactly once.
	<-svc.renderSlots
	jobRetry, _, err := svc.CreateRender(context.Background(), "render_cap_b_01", RenderWriteRequest{TimelineID: planB.ID})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if jobRetry.State != domain.RenderCompleted {
		t.Fatalf("retried job not completed: %s", jobRetry.State)
	}
	_ = planA
}

func TestSingleRenderIdRunsOnce(t *testing.T) {
	config := DefaultConfig()
	config.MaxParallelRenders = 4
	svc, _ := newTestService(t, config)
	plan := seedTimeline(t, svc, "tl_once_001", 24000)

	const callers = 8
	var wg sync.WaitGroup
	results := make([]domain.RenderJob, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			job, _, err := svc.CreateRender(context.Background(), "render_once_01", RenderWriteRequest{TimelineID: plan.ID})
			results[idx] = job
			errs[idx] = err
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].State != domain.RenderCompleted {
			t.Fatalf("caller %d state %s", i, results[i].State)
		}
	}
	// Exactly one committed output with a stable digest across all callers.
	firstDigest := results[0].ReceiptDigest
	for _, r := range results {
		if r.ReceiptDigest != firstDigest || r.Output.Digest != firstDigest {
			t.Fatal("contradictory render results for the same id")
		}
	}
}

func TestRepeatedRenderIsDeterministic(t *testing.T) {
	svc, _ := newTestService(t, DefaultConfig())
	plan := seedTimeline(t, svc, "tl_det_001", 12000)
	first, _, err := svc.CreateRender(context.Background(), "render_det_01", RenderWriteRequest{TimelineID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Re-PUT the same id returns the same completed job (idempotent), not a rerun.
	again, _, err := svc.CreateRender(context.Background(), "render_det_01", RenderWriteRequest{TimelineID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	if again.Output.Digest != first.Output.Digest || again.State != domain.RenderCompleted {
		t.Fatal("repeated render changed the output")
	}
}

func TestTwoRendersRunInParallel(t *testing.T) {
	config := DefaultConfig()
	config.MaxParallelRenders = 2
	svc, _ := newTestService(t, config)
	planA := seedTimeline(t, svc, "tl_par_a_01", 24000)
	planB := seedTimeline(t, svc, "tl_par_b_01", 24000)

	var wg sync.WaitGroup
	states := make([]domain.RenderState, 2)
	for i, spec := range []struct {
		id, tl string
	}{
		{"render_par_a_01", planA.ID},
		{"render_par_b_01", planB.ID},
	} {
		wg.Add(1)
		go func(idx int, id, tl string) {
			defer wg.Done()
			job, _, err := svc.CreateRender(context.Background(), id, RenderWriteRequest{TimelineID: tl})
			if err != nil {
				t.Errorf("render %s: %v", id, err)
				return
			}
			states[idx] = job.State
		}(i, spec.id, spec.tl)
	}
	wg.Wait()
	for i, state := range states {
		if state != domain.RenderCompleted {
			t.Fatalf("render %d did not complete: %s", i, state)
		}
	}
}

func TestMemoryBudgetRejectsOversizedJob(t *testing.T) {
	config := DefaultConfig()
	// An artificially tiny budget makes even a small render inadmissible.
	config.RenderMemoryBytes = 1 << 20
	svc, _ := newTestService(t, config)
	plan := seedTimeline(t, svc, "tl_mem_001", 12000)
	job, _, err := svc.CreateRender(context.Background(), "render_mem_01", RenderWriteRequest{TimelineID: plan.ID})
	if err == nil {
		t.Fatal("expected budget failure")
	}
	appErr := domain.AsAppError(err)
	if appErr.Detail.Code != "render_memory_budget_exceeded" {
		t.Fatalf("unexpected code %s", appErr.Detail.Code)
	}
	if job.State != domain.RenderFailed {
		t.Fatalf("oversized job should fail terminally, got %s", job.State)
	}
}

func TestRecoveryAdoptsQueuedJob(t *testing.T) {
	config := DefaultConfig()
	svc, repo := newTestService(t, config)
	plan := seedTimeline(t, svc, "tl_rec_001", 12000)

	// Durably create a queued job as if a previous process died before running.
	queued := domain.RenderJob{
		ID: "render_rec_01", TimelineID: plan.ID, State: domain.RenderQueued,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := repo.SaveJob(queued); err != nil {
		t.Fatal(err)
	}
	svc.RecoverInterruptedJobs(context.Background())

	got, err := svc.GetRender(context.Background(), "render_rec_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RenderCompleted || got.Output == nil {
		t.Fatalf("recovery did not complete job: state=%s", got.State)
	}
}

func TestReconcilesOrphanedArtifacts(t *testing.T) {
	svc, repo := newTestService(t, DefaultConfig())
	plan := seedTimeline(t, svc, "tl_orph_001", 12000)

	// Run a real render, then simulate a crash that rewound the job state to
	// finalizing while leaving the committed output folder in place.
	job, _, err := svc.CreateRender(context.Background(), "render_orph_01", RenderWriteRequest{TimelineID: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := job.Output.Digest
	rewound := job
	rewound.State = domain.RenderFinalizing
	rewound.Output = nil
	rewound.ReceiptDigest = ""
	if err := repo.SaveJob(rewound); err != nil {
		t.Fatal(err)
	}

	// Recovery must adopt the existing output rather than fail on duplicate
	// publication or produce a contradictory receipt.
	svc.RecoverInterruptedJobs(context.Background())
	got, err := svc.GetRender(context.Background(), "render_orph_01")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RenderCompleted {
		t.Fatalf("orphaned job not reconciled to completed: %s", got.State)
	}
	if got.Output == nil || got.Output.Digest != wantDigest {
		t.Fatalf("reconciled output digest mismatch")
	}
}
