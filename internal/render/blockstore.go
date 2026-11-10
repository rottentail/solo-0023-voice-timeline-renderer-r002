package render

import (
	"errors"
	"io"
	"os"
)

// blockStore holds a large frame-addressed float32 mix buffer while keeping
// only a bounded number of blocks resident in memory. Evicted blocks are
// spilled to a scratch file and read back on demand, so render memory stays
// flat no matter how long the timeline is.
//
// Bit-exactness strategy: the mixer processes spans in their original order, so
// the float32 add/multiply sequence for every sample is unchanged. A block that
// still receives future contributions (a crossfade region) is pinned and kept
// resident until its last span has been applied; only fully finalized blocks
// are evicted. A block is therefore never modified after being reloaded, which
// keeps eviction from changing any rounding result. Each render owns a private
// store; access is single-goroutine and needs no locking.
type blockStore struct {
	file          *os.File
	channels      int
	blockFrames   int64
	residentLimit int

	blockCount int
	frameCount int64
	resident   map[int][]float32
	pinned     map[int]bool
	lru        []int // unpinned resident block indices, oldest first
	flushed    map[int]bool
}

func newBlockStore(scratch *os.File, channels int, blockFrames int64, residentLimit int) *blockStore {
	return &blockStore{
		file:          scratch,
		channels:      channels,
		blockFrames:   blockFrames,
		residentLimit: residentLimit,
		resident:      make(map[int][]float32),
		pinned:        make(map[int]bool),
		flushed:       make(map[int]bool),
	}
}

// blockFor returns the resident block backing frame, loading it from scratch if
// needed. Pinned blocks are exempt from eviction.
func (store *blockStore) blockFor(frame int64) ([]float32, int) {
	index := int(frame / store.blockFrames)
	offset := int(frame%store.blockFrames) * store.channels
	if block, ok := store.resident[index]; ok {
		store.touch(index)
		if frame+1 > store.frameCount {
			store.frameCount = frame + 1
		}
		return block, offset
	}
	block := make([]float32, int(store.blockFrames)*store.channels)
	if store.flushed[index] {
		store.readBlock(index, block)
	}
	if index >= store.blockCount {
		store.blockCount = index + 1
	}
	if frame+1 > store.frameCount {
		store.frameCount = frame + 1
	}
	store.resident[index] = block
	if !store.pinned[index] {
		store.lru = append(store.lru, index)
	}
	store.evictIfNeeded()
	return block, offset
}

// addSample accumulates one mixed sample at the absolute output position.
func (store *blockStore) addSample(frame, channel int64, value float32) {
	block, offset := store.blockFor(frame)
	block[offset+int(channel)] += value
}

// scaleSample applies the outgoing crossfade weight in place before the
// incoming sample is added, matching the original mixer.
func (store *blockStore) scaleSample(frame, channel int64, factor float32) {
	block, offset := store.blockFor(frame)
	block[offset+int(channel)] *= factor
}

// Pin keeps the block range covering [startFrame, endFrame) resident until
// Unpin is called. Used for crossfade regions that later spans must modify.
func (store *blockStore) PinRange(startFrame, endFrame int64) {
	first := int(startFrame / store.blockFrames)
	last := int((endFrame - 1) / store.blockFrames)
	for index := first; index <= last; index++ {
		if store.pinned[index] {
			continue
		}
		store.pinned[index] = true
		store.removeLRU(index)
		// Ensure the pinned block is resident immediately.
		if _, ok := store.resident[index]; !ok {
			block := make([]float32, int(store.blockFrames)*store.channels)
			if store.flushed[index] {
				store.readBlock(index, block)
			}
			store.resident[index] = block
			if index >= store.blockCount {
				store.blockCount = index + 1
			}
		}
	}
}

// Unpin releases the pin on a block range, allowing eviction again.
func (store *blockStore) UnpinRange(startFrame, endFrame int64) {
	first := int(startFrame / store.blockFrames)
	last := int((endFrame - 1) / store.blockFrames)
	for index := first; index <= last; index++ {
		if !store.pinned[index] {
			continue
		}
		delete(store.pinned, index)
		if _, ok := store.resident[index]; ok {
			store.insertLRU(index)
		}
	}
	store.evictIfNeeded()
}

// SequentialReader walks the whole store from frame zero during the final
// normalization/encode scan.
func (store *blockStore) SequentialReader() *sequentialReader {
	return &sequentialReader{store: store}
}

type sequentialReader struct {
	store    *blockStore
	blockIdx int
}

func (reader *sequentialReader) NextBlock() ([]float32, int, error) {
	if reader.blockIdx >= reader.store.blockCount {
		return nil, 0, io.EOF
	}
	index := reader.blockIdx
	block, _ := reader.store.blockFor(int64(index) * reader.store.blockFrames)
	reader.blockIdx++
	validFrames := int(reader.store.blockFrames)
	if index == reader.store.blockCount-1 {
		if remainder := reader.store.frameCount % reader.store.blockFrames; remainder != 0 {
			validFrames = int(remainder)
		}
	}
	return block, validFrames, nil
}

func (store *blockStore) FrameCount() int64 { return store.frameCount }

func (store *blockStore) SetFrameCount(frames int64) {
	store.frameCount = frames
	blocks := int((frames + store.blockFrames - 1) / store.blockFrames)
	if blocks > store.blockCount {
		store.blockCount = blocks
	}
}

func (store *blockStore) Close() error {
	if store.file == nil {
		return nil
	}
	err := store.file.Close()
	store.file = nil
	return err
}

func (store *blockStore) touch(index int) {
	if store.pinned[index] {
		return
	}
	store.removeLRU(index)
	store.lru = append(store.lru, index)
}

func (store *blockStore) removeLRU(index int) {
	for position, candidate := range store.lru {
		if candidate == index {
			store.lru = append(store.lru[:position], store.lru[position+1:]...)
			return
		}
	}
}

func (store *blockStore) insertLRU(index int) {
	store.removeLRU(index)
	store.lru = append(store.lru, index)
}

// evictIfNeeded spills unpinned resident blocks beyond the limit. Pinned blocks
// are never evicted, so the hard memory cap is residentLimit blocks plus the
// number of concurrently pinned blocks (bounded by crossfade size).
func (store *blockStore) evictIfNeeded() {
	for len(store.resident)-len(store.pinned) > store.residentLimit && len(store.lru) > 0 {
		victim := store.lru[0]
		store.lru = store.lru[1:]
		block := store.resident[victim]
		delete(store.resident, victim)
		store.writeBlock(victim, block)
		store.flushed[victim] = true
	}
}

func (store *blockStore) blockOffsetBytes(index int) int64 {
	return int64(index) * store.blockFrames * int64(store.channels) * 4
}

func (store *blockStore) writeBlock(index int, block []float32) {
	if _, err := store.file.WriteAt(float32SliceBytes(block), store.blockOffsetBytes(index)); err != nil {
		panic(errScratchIO(err))
	}
}

func (store *blockStore) readBlock(index int, block []float32) {
	if _, err := store.file.ReadAt(float32SliceBytes(block), store.blockOffsetBytes(index)); err != nil && !errors.Is(err, io.EOF) {
		panic(errScratchIO(err))
	}
}

func errScratchIO(err error) error {
	return errors.New("render scratch storage failed: " + err.Error())
}
