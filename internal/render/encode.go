package render

import (
	"context"
	"encoding/binary"
	"io"
	"math"

	"voice-timeline-renderer/internal/audiocodec"
	"voice-timeline-renderer/internal/domain"
)

// errFrameCountMismatch guards against a block store whose frame total disagrees
// with the validated placement plan.
var errFrameCountMismatch = newRenderError("encoded frame count does not match the timeline plan")

type renderError string

func (err renderError) Error() string { return string(err) }

func newRenderError(message string) error { return renderError(message) }

// countingWriter folds PCM data into the file + digest and tracks the data
// chunk size needed for the WAV header patch.
type countingWriter struct {
	output *fileOutput
}

func (writer countingWriter) Write(p []byte) (int, error) {
	n, err := writer.output.hashingTarget().Write(p)
	writer.output.recordData(n)
	return n, err
}

// encodingWriter applies the normalization gain and streams quantized PCM16
// bytes into the sink. Clamping and quantization are copied verbatim from the
// original EncodePCM16 path, so output bytes are identical.
type encodingWriter struct {
	target    io.Writer
	metrics   *audiocodec.MetricsAccumulator
	channels  int
	gain      float64
	byteCount int64
}

func (stream *encodingWriter) write(samples []float32) error {
	buffer := make([]byte, len(samples)*2)
	quantized := make([]float32, len(samples))
	for index, sample := range samples {
		// Match the original normalizePeak exactly: multiply in float64, clamp,
		// then round to float32 before quantization.
		value := float64(sample) * stream.gain
		if value > 1 {
			value = 1
		}
		if value < -1 {
			value = -1
		}
		normalized := float32(value)
		quantized[index] = normalized
		var code int32
		switch {
		case normalized <= -1:
			code = -32768
		case normalized >= 1:
			code = 32767
		case normalized < 0:
			code = int32(math.Round(float64(normalized) * 32768))
		default:
			code = int32(math.Round(float64(normalized) * 32767))
		}
		if code < -32768 {
			code = -32768
		}
		if code > 32767 {
			code = 32767
		}
		binary.LittleEndian.PutUint16(buffer[index*2:index*2+2], uint16(int16(code)))
	}
	// Output metrics in the original receipt were measured by decoding the
	// finished WAV, i.e. over the quantized int16 values mapped back to
	// float32. Reconstruct those exact values here without a decode pass.
	for index := range quantized {
		code := int16(binary.LittleEndian.Uint16(buffer[index*2 : index*2+2]))
		if code < 0 {
			quantized[index] = float32(code) / 32768
		} else {
			quantized[index] = float32(code) / 32767
		}
	}
	stream.metrics.Add(quantized)
	n, err := stream.target.Write(buffer)
	stream.byteCount += int64(n)
	return err
}

// encodeNormalized performs the second sequential pass over the block store:
// apply the normalization gain, quantize to PCM16, stream into the sink, and
// accumulate the output metrics and digest over the published bytes.
func encodeNormalized(
	ctx context.Context,
	store *blockStore,
	output *fileOutput,
	sampleRate, channels int,
	totalFrames int64,
	normalization normalizationResult,
) (domain.OutputAsset, audiocodec.Metrics, error) {
	stream := &encodingWriter{
		target:   countingWriter{output: output},
		metrics:  &audiocodec.MetricsAccumulator{},
		channels: channels,
		gain:     normalization.gain,
	}
	reader := store.SequentialReader()
	writtenFrames := int64(0)
	for {
		if writtenFrames%int64(4096*64) == 0 {
			select {
			case <-ctx.Done():
				return domain.OutputAsset{}, audiocodec.Metrics{}, ctx.Err()
			default:
			}
		}
		block, validFrames, err := reader.NextBlock()
		if err == io.EOF {
			break
		}
		if err != nil {
			return domain.OutputAsset{}, audiocodec.Metrics{}, err
		}
		if err := stream.write(block[:validFrames*channels]); err != nil {
			return domain.OutputAsset{}, audiocodec.Metrics{}, err
		}
		writtenFrames += int64(validFrames)
	}
	if writtenFrames != totalFrames {
		return domain.OutputAsset{}, audiocodec.Metrics{}, errFrameCountMismatch
	}
	outputMetrics := stream.metrics.Metrics()
	asset := domain.OutputAsset{
		// The digest is finalized in Close after the header size patch.
		SampleRate:    sampleRate,
		Channels:      channels,
		BitsPerSample: 16,
		FrameCount:    writtenFrames,
		DurationMS:    framesToMilliseconds(writtenFrames, sampleRate),
		PeakDBFS:      outputMetrics.PeakDBFS,
		RMSDBFS:       outputMetrics.RMSDBFS,
	}
	return asset, outputMetrics, nil
}

const wavHeaderBytes = 44
