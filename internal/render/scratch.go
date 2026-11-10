package render

import (
	"os"
)

// openScratch creates the private spill file used by one render. It lives in
// the repository tmp directory so block eviction never consumes anonymous
// memory, and is always removed by the caller on completion or failure.
func openScratch(scratchDir string) (*os.File, error) {
	if err := os.MkdirAll(scratchDir, 0o750); err != nil {
		return nil, err
	}
	handle, err := os.CreateTemp(scratchDir, "mix-spill-*")
	if err != nil {
		return nil, err
	}
	return handle, nil
}

func removeScratch(name string) error {
	if name == "" {
		return nil
	}
	return os.Remove(name)
}
