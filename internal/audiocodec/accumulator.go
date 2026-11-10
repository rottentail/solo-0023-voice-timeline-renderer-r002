package audiocodec

import "math"

// MetricsAccumulator computes peak and RMS over an arbitrary stream of
// samples, matching Measure exactly while avoiding holding every sample in
// memory. RMS is derived from the per-sample mean of squared amplitudes, which
// is order-independent, so incremental accumulation yields the same result as
// the whole-buffer computation.
type MetricsAccumulator struct {
	peak       float64
	sumSquares float64
	count      int
}

func (acc *MetricsAccumulator) Add(samples []float32) {
	for _, sample := range samples {
		value := float64(sample)
		absolute := math.Abs(value)
		if absolute > acc.peak {
			acc.peak = absolute
		}
		acc.sumSquares += value * value
		acc.count++
	}
}

func (acc *MetricsAccumulator) Metrics() Metrics {
	if acc.count == 0 {
		return Metrics{PeakDBFS: silenceFloorDBFS, RMSDBFS: silenceFloorDBFS}
	}
	rms := math.Sqrt(acc.sumSquares / float64(acc.count))
	return Metrics{
		PeakDBFS: amplitudeDBFS(acc.peak),
		RMSDBFS:  amplitudeDBFS(rms),
	}
}

func (acc *MetricsAccumulator) PeakLinear() float64 {
	return acc.peak
}
