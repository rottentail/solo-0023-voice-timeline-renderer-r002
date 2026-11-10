package storage

import (
	"errors"
	"os"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

func (repo *Repository) SaveJob(job domain.RenderJob) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	path := repo.path("jobs", job.ID+".json")
	if err := writeJSONAtomic(path, job, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write render job", err)
	}
	return nil
}

func (repo *Repository) GetJob(id string) (domain.RenderJob, error) {
	var job domain.RenderJob
	if err := readJSON(repo.path("jobs", id+".json"), &job); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.RenderJob{}, domain.E(domain.KindNotFound, "render_not_found", "render_id", "render job does not exist")
		}
		return domain.RenderJob{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render job", err)
	}
	return job, nil
}

// NewRenderSink builds the streaming sink for one render. The sink stages all
// bytes outside the repository lock and only takes the lock for the atomic
// publication rename.
func (repo *Repository) NewRenderSink(renderID string) render.Sink {
	return newRenderSink(repo, renderID)
}

// ScratchDir returns the tmp directory used for per-render block spill files.
func (repo *Repository) ScratchDir() string {
	return repo.path("tmp")
}

func (repo *Repository) GetRenderOutput(id string) ([]byte, domain.RenderReceipt, error) {
	var receipt domain.RenderReceipt
	if err := readJSON(repo.path("renders", id, "receipt.json"), &receipt); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return nil, domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render receipt", err)
	}
	payload, err := os.ReadFile(repo.path("renders", id, "output.wav"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.RenderReceipt{}, domain.E(domain.KindUnready, "render_not_completed", "render_id", "render output is not available")
		}
		return nil, domain.RenderReceipt{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read render output", err)
	}
	return payload, receipt, nil
}

// ListJobIDs returns every stored render job identifier, used at startup to
// adopt jobs interrupted by a process restart.
func (repo *Repository) ListJobIDs() ([]string, error) {
	entries, err := os.ReadDir(repo.path("jobs"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot list render jobs", err)
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if len(name) > 5 && name[len(name)-5:] == ".json" {
			ids = append(ids, name[:len(name)-5])
		}
	}
	return ids, nil
}
