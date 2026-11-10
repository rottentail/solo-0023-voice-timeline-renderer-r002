package storage

import (
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/domain"
	"voice-timeline-renderer/internal/render"
)

// renderSink implements render.Sink against the local repository. Streaming
// happens entirely outside the repository lock; the lock is taken only for the
// final existence check and atomic rename, so a long render never blocks
// source or timeline reads.
type renderSink struct {
	repo      *Repository
	renderID  string
	stageDir  string
	dataFile  *os.File
	published bool
}

func newRenderSink(repo *Repository, renderID string) *renderSink {
	return &renderSink{repo: repo, renderID: renderID}
}

func (sink *renderSink) NewOutput(sampleRate, channels int) (*render.FileOutput, error) {
	if err := os.MkdirAll(sink.repo.path("tmp"), 0o750); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(sink.repo.path("tmp"), "render-*")
	if err != nil {
		return nil, err
	}
	sink.stageDir = dir
	file, err := os.OpenFile(filepath.Join(dir, "output.wav"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o640)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	sink.dataFile = file
	return render.NewFileOutput(file, sampleRate, channels)
}

func (sink *renderSink) Commit(output *render.FileOutput, receipt domain.RenderReceipt) error {
	receiptPath := filepath.Join(sink.stageDir, "receipt.json")
	if err := writeJSONAtomic(receiptPath, receipt, 0o640); err != nil {
		return err
	}
	final := sink.repo.path("renders", sink.renderID)

	sink.repo.mu.Lock()
	defer sink.repo.mu.Unlock()
	if _, err := os.Stat(final); err == nil {
		return domain.E(domain.KindConflict, "render_conflict", "render_id", "render artifacts already exist")
	} else if !os.IsNotExist(err) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect render destination", err)
	}
	if err := os.Rename(sink.stageDir, final); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit render artifacts", err)
	}
	if err := syncDirectory(sink.repo.path("renders")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync render index", err)
	}
	sink.published = true
	sink.stageDir = ""
	return nil
}

func (sink *renderSink) Abort(output *render.FileOutput) {
	if output != nil {
		_ = output.CloseFile()
	}
	if sink.stageDir != "" {
		_ = os.RemoveAll(sink.stageDir)
		sink.stageDir = ""
	}
}
