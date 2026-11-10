package render

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"os"
)

// fileOutput is a staged WAV destination. It writes the canonical 44-byte
// header up front, streams PCM16 data while hashing every byte, and patches the
// RIFF/data sizes on close so the file is identical to one produced by
// EncodePCM16. Nothing is visible at the published path until the sink commits
// the staging directory.
type fileOutput struct {
	file       *os.File
	digest     hash.Hash
	dataBytes  int64
	header     [44]byte
	sampleRate int
	channels   int
	closed     bool
}

// FileOutput is the exported alias used by storage-side sinks.
type FileOutput = fileOutput

// NewFileOutput creates a WAV output backed by an already-open temp file.
func NewFileOutput(file *os.File, sampleRate, channels int) (*fileOutput, error) {
	output := &fileOutput{
		file:       file,
		digest:     sha256.New(),
		sampleRate: sampleRate,
		channels:   channels,
	}
	header := buildWAVHeader(sampleRate, channels)
	output.header = header
	if _, err := file.Write(header[:]); err != nil {
		return nil, err
	}
	output.digest.Write(header[:])
	return output, nil
}

func buildWAVHeader(sampleRate, channels int) [44]byte {
	var header [44]byte
	blockAlign := channels * 2
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 0)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], uint32(sampleRate*blockAlign))
	binary.LittleEndian.PutUint16(header[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(header[34:36], 16)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], 0)
	return header
}

// hashingTarget returns the writer used for PCM data: every data byte is both
// persisted and folded into the receipt digest.
func (output *fileOutput) hashingTarget() io.Writer {
	return io.MultiWriter(output.file, output.digest)
}

// Close patches the RIFF and data size fields, syncs the file, and re-hashes
// the complete final byte sequence. The digest therefore covers exactly the
// final on-disk bytes, matching sha256 over an EncodePCM16 buffer.
func (output *fileOutput) Close() error {
	if output.closed {
		return nil
	}
	output.closed = true
	patched := output.header
	binary.LittleEndian.PutUint32(patched[4:8], uint32(output.dataBytes+36))
	binary.LittleEndian.PutUint32(patched[40:44], uint32(output.dataBytes))
	if _, err := output.file.WriteAt(patched[4:8], 4); err != nil {
		return err
	}
	if _, err := output.file.WriteAt(patched[40:44], 40); err != nil {
		return err
	}
	output.digest.Reset()
	if _, err := output.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(output.digest, output.file); err != nil {
		return err
	}
	if _, err := output.file.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return output.file.Sync()
}

func (output *fileOutput) recordData(n int) {
	output.dataBytes += int64(n)
}

func (output *fileOutput) bytesWritten() int64 {
	return output.dataBytes + wavHeaderBytes
}

// DigestHex returns the hex digest over the final on-disk WAV bytes. It is
// valid only after Close has patched and re-hashed the container.
func (output *fileOutput) DigestHex() string {
	return hex.EncodeToString(output.digest.Sum(nil))
}

// File exposes the staged handle for the sink's atomic publication step.
func (output *fileOutput) File() *os.File {
	return output.file
}

// CloseFile releases the underlying descriptor without further patching. It is
// used by sink abort paths where the staged file will be removed anyway.
func (output *fileOutput) CloseFile() error {
	if output.file == nil {
		return nil
	}
	err := output.file.Close()
	output.file = nil
	return err
}
