package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/domain"
)

// SaveSource stages the source folder outside the lock and only holds the
// repository lock for the final existence check and atomic rename. Media
// streaming therefore never blocks concurrent reads.
func (repo *Repository) SaveSource(asset domain.SourceAsset, wav []byte) error {
	final := repo.path("sources", asset.ID)
	repo.mu.RLock()
	_, statErr := os.Stat(final)
	repo.mu.RUnlock()
	if statErr == nil {
		return domain.E(domain.KindConflict, "source_conflict", "source_id", "source identifier already exists")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect source destination", statErr)
	}

	temp, err := os.MkdirTemp(repo.path("tmp"), "source-*")
	if err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create source staging directory", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temp)
		}
	}()
	if err := writeBytes(filepath.Join(temp, "source.wav"), wav, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write source media", err)
	}
	if err := writeJSONAtomic(filepath.Join(temp, "metadata.json"), asset, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write source metadata", err)
	}

	repo.mu.Lock()
	if _, err := os.Stat(final); err == nil {
		repo.mu.Unlock()
		return domain.E(domain.KindConflict, "source_conflict", "source_id", "source identifier already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		repo.mu.Unlock()
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect source destination", err)
	}
	if err := os.Rename(temp, final); err != nil {
		repo.mu.Unlock()
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit source", err)
	}
	if err := syncDirectory(repo.path("sources")); err != nil {
		repo.mu.Unlock()
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync source index", err)
	}
	repo.mu.Unlock()
	committed = true
	return nil
}

func (repo *Repository) GetSourceMeta(id string) (domain.SourceAsset, error) {
	var asset domain.SourceAsset
	if err := readJSON(repo.path("sources", id, "metadata.json"), &asset); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.SourceAsset{}, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source does not exist")
		}
		return domain.SourceAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read source metadata", err)
	}
	return asset, nil
}

func (repo *Repository) GetSource(id string) (domain.SourceAsset, []byte, error) {
	asset, err := repo.GetSourceMeta(id)
	if err != nil {
		return domain.SourceAsset{}, nil, err
	}
	payload, readErr := os.ReadFile(repo.path("sources", id, "source.wav"))
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return domain.SourceAsset{}, nil, domain.E(domain.KindNotFound, "source_not_found", "source_id", "source media does not exist")
		}
		return domain.SourceAsset{}, nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read source media", readErr)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != asset.Digest {
		return domain.SourceAsset{}, nil, domain.E(domain.KindInternal, "source_integrity_error", "source_id", "stored source digest does not match metadata")
	}
	return asset, payload, nil
}
