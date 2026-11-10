package render

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

type normalizationResult struct {
	Before audiocodec.Metrics
	GainDB float64
	gain   float64
}

// planNormalization computes the single deterministic peak-normalization gain
// from the raw pre-mix linear peak, exactly matching the original normalizePeak
// path which divided the target by the measured linear peak. Applying the gain
// lazily during encoding produces the same float32 values without a second
// full-size sample buffer.
func planNormalization(before audiocodec.Metrics, peakLinear float64, targetDBFS float64) normalizationResult {
	target := audiocodec.DBToLinear(targetDBFS)
	gain := 1.0
	if peakLinear > 0 {
		gain = target / peakLinear
	}
	if math.IsNaN(gain) || math.IsInf(gain, 0) || gain <= 0 {
		gain = 1
	}
	return normalizationResult{
		Before: before,
		GainDB: audiocodec.LinearToDB(gain),
		gain:   gain,
	}
}
