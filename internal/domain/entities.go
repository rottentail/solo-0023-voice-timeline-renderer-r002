package domain

import "time"

type SourceAsset struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	ByteCount     int64     `json:"byte_count"`
	SampleRate    int       `json:"sample_rate"`
	Channels      int       `json:"channels"`
	BitsPerSample int       `json:"bits_per_sample"`
	FrameCount    int64     `json:"frame_count"`
	DurationMS    int64     `json:"duration_ms"`
	PeakDBFS      float64   `json:"peak_dbfs"`
	RMSDBFS       float64   `json:"rms_dbfs"`
	CreatedAt     time.Time `json:"created_at"`
}

type TimelineSpan struct {
	SourceID         string  `json:"source_id"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	GainDB           float64 `json:"gain_db"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
}

type Timeline struct {
	ID             string         `json:"id"`
	Name           string         `json:"name"`
	SampleRate     int            `json:"sample_rate"`
	OutputChannels int            `json:"output_channels"`
	TargetPeakDBFS float64        `json:"target_peak_dbfs"`
	Spans          []TimelineSpan `json:"spans"`
	State          string         `json:"state"`
	OutputFrames   int64          `json:"output_frames"`
	CreatedAt      time.Time      `json:"created_at"`
}

type RenderState string

const (
	RenderQueued     RenderState = "queued"
	RenderRendering  RenderState = "rendering"
	RenderFinalizing RenderState = "finalizing"
	RenderCompleted  RenderState = "completed"
	RenderFailed     RenderState = "failed"
)

type OutputAsset struct {
	Digest        string  `json:"digest"`
	ByteCount     int64   `json:"byte_count"`
	SampleRate    int     `json:"sample_rate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bits_per_sample"`
	FrameCount    int64   `json:"frame_count"`
	DurationMS    int64   `json:"duration_ms"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
}

type RenderJob struct {
	ID            string       `json:"id"`
	TimelineID    string       `json:"timeline_id"`
	State         RenderState  `json:"state"`
	Error         *ErrorDetail `json:"error,omitempty"`
	Output        *OutputAsset `json:"output,omitempty"`
	ReceiptDigest string       `json:"receipt_digest,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
}

type ReceiptSpan struct {
	Index            int     `json:"index"`
	SourceID         string  `json:"source_id"`
	SourceDigest     string  `json:"source_digest"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	OutputStartFrame int64   `json:"output_start_frame"`
	OutputEndFrame   int64   `json:"output_end_frame"`
	OutputFrames     int64   `json:"output_frames"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
	GainDB           float64 `json:"gain_db"`
	ChannelAction    string  `json:"channel_action"`
}

type NormalizationReceipt struct {
	RequestedPeakDBFS float64 `json:"requested_peak_dbfs"`
	BeforePeakDBFS    float64 `json:"before_peak_dbfs"`
	AppliedGainDB     float64 `json:"applied_gain_db"`
	OutputPeakDBFS    float64 `json:"output_peak_dbfs"`
}

type RenderReceipt struct {
	SchemaVersion int                  `json:"schema_version"`
	RenderID      string               `json:"render_id"`
	TimelineID    string               `json:"timeline_id"`
	Output        OutputAsset          `json:"output"`
	Normalization NormalizationReceipt `json:"normalization"`
	Spans         []ReceiptSpan        `json:"spans"`
	Warnings      []string             `json:"warnings"`
	CreatedAt     time.Time            `json:"created_at"`
}

func ValidRenderTransition(from, to RenderState) bool {
	switch from {
	case RenderQueued:
		return to == RenderRendering || to == RenderFailed
	case RenderRendering:
		return to == RenderFinalizing || to == RenderFailed
	case RenderFinalizing:
		return to == RenderCompleted || to == RenderFailed
	default:
		return false
	}
}

func (job *RenderJob) Transition(to RenderState, detail *ErrorDetail, now time.Time) error {
	if job.State == to {
		return nil
	}
	if !ValidRenderTransition(job.State, to) {
		return E(
			KindConflict,
			"invalid_state_transition",
			"state",
			"render job cannot move from "+string(job.State)+" to "+string(to),
		)
	}
	if to == RenderFailed && detail == nil {
		return E(KindInternal, "missing_failure_detail", "error", "a failed render requires an error detail")
	}
	if to != RenderFailed {
		detail = nil
	}
	job.State = to
	job.Error = detail
	job.UpdatedAt = now.UTC()
	return nil
}

// ReAdopt moves a job left behind by an interrupted process back to rendering so
// the deterministic pipeline can run to completion. Completed and failed jobs
// are terminal and can never be adopted.
func (job *RenderJob) ReAdopt(now time.Time) error {
	if job.State == RenderCompleted || job.State == RenderFailed {
		return E(KindConflict, "invalid_state_transition", "state", "terminal render job cannot be adopted")
	}
	job.State = RenderRendering
	job.Error = nil
	job.UpdatedAt = now.UTC()
	return nil
}
