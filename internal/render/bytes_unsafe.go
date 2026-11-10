package render

import "unsafe"

// float32SliceBytes reinterprets a float32 slice as its little-endian byte
// representation for scratch-file spill and reload. All supported Go platforms
// are little-endian; the size assertion keeps the unsafe use honest.
func float32SliceBytes(values []float32) []byte {
	const floatSize = 4
	if len(values) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&values[0])), len(values)*floatSize)
}
