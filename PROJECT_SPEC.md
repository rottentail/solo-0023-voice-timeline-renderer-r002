# Voice Timeline Renderer

## Service goal

Voice Timeline Renderer is a backend service for assembling narrated PCM audio
into a deterministic mixdown. It gives audio-platform engineers a small HTTP
surface for registering 16-bit WAV sources, composing ordered timeline spans,
and rendering a normalized output along with a machine-readable render
receipt.

The project is intentionally production-only. Automated tests, fixtures, and a
test command are deferred to the downstream task stage, which will add focused
unit, integration, and end-to-end verification.

## Users

- Audio-platform engineers who need a self-hosted assembly stage.
- Audio editing tooling that submits WAV sources and timeline operations.
- Quality engineers who replay a render and compare the receipt with the output.

## Project shape and stack

- Shape: backend.
- Language: Go.
- Runtime: Go bundled HTTP packages.
- Persistence: a local data directory containing metadata JSON and media files.
- External services: none.

## Core entities

### Source asset

A source asset is an immutable uploaded PCM WAV. Its identity, SHA-256 digest,
frame count, sample rate, channel count, peak amplitude, RMS amplitude, and duration are
recorded after decoding. A source is never updated in place.

Invariants:

- The payload must be a RIFF/WAVE file with 16-bit integer PCM samples.
- Only mono and stereo layouts are accepted.
- The digest and signal measurements describe the exact bytes stored.
- Decoded payloads are bounded by configured byte and duration limits.

### Assembly timeline

A timeline is an ordered set of spans over existing source assets. A span
selects a half-open source frame interval, a gain in decibels, and an optional
crossfade with the preceding span.

Invariants:

- A timeline has at least one span.
- Every referenced source exists.
- Source intervals are non-empty and remain inside the source frame count.
- The first span has no crossfade.
- A crossfade cannot exceed the shorter neighboring span.
- Output channel count is explicit and independent from an input source's
  channel count.

### Render job

A render job is the durable record of one timeline execution.

State machine:

`queued -> rendering -> finalizing -> completed`

Any active state may transition to `failed`. `completed` and `failed` are
terminal. A failed job stores a stable error code and message. A completed job
stores the output file, output metadata, and immutable render receipt.

A job that cannot start because the worker pool or memory budget is saturated
stays durably `queued` and the request returns a retryable `unavailable`
error (`render_capacity_exhausted`, HTTP 503 with `Retry-After`). Resubmitting
the same render identifier resumes the queued job and never publishes a second
output or receipt. A job whose own footprint exceeds the whole render memory
budget fails terminally with `render_memory_budget_exceeded`.

### Render receipt

A receipt records source identifiers, span placement, gain adjustments, peak
and RMS measurements, final normalization gain, frame count, output digest, and
non-fatal warnings. It is written only after the output bytes have been
atomically committed.

## Workflows

### 1. Register audio source

Entry point: `PUT /v1/sources/{source_id}`.

1. The HTTP boundary enforces request size and base64 decoding.
2. The service validates identity fields and decodes the WAV structure.
3. The codec layer verifies PCM format, frame alignment, and signal bounds.
4. Signal measurements are computed from the decoded samples.
5. Storage writes the media bytes and metadata using an atomic commit.

Success returns source metadata. Failure returns a stable validation error and
leaves no visible partial source.

### 2. Compose assembly timeline

Entry point: `PUT /v1/timelines/{timeline_id}`.

1. The HTTP boundary decodes and sizes the span list.
2. Domain validation checks ranges, gain limits, crossfade limits, and temporal
   ordering.
3. The service resolves every source reference from storage.
4. A timeline record is persisted once all spans are valid.

Success returns a ready timeline. Failure reports the first stable validation
issue without persisting a partial timeline.

### 3. Render timeline mixdown

Entry point: `PUT /v1/renders/{render_id}` with a `timeline_id` body field.

1. The service creates a durable `queued` job.
2. Admission reserves a worker slot and a bounded memory reservation; on
   saturation the job stays `queued` and a retryable capacity error is returned.
3. The renderer moves to `rendering` and loads referenced sources on demand,
   keeping only a small source cache resident.
4. Each source interval is resampled to the timeline sample rate, mapped to the
   requested channel layout, gain-adjusted, faded, and placed on the timeline.
   The mix accumulates in fixed-size float32 blocks; crossfade regions stay
   resident while they still receive contributions, and finished blocks spill
   to a private scratch file, so memory is independent of timeline length.
5. Overlapping neighboring spans are mixed with equal-power crossfade weights.
6. The finalizing stage applies peak normalization to the requested target,
   streams a WAV and receipt through temporary files, and renames them into
   place atomically.
7. The job moves to `completed`. Any error records a failed terminal state and
   removes unpublished temporary output.

Success is observable through the job resource, the downloaded WAV, and the
receipt. Failure is observable through the terminal job error code.

## HTTP interface

- `GET /healthz`
- `PUT /v1/sources/{source_id}`
- `GET /v1/sources/{id}`
- `PUT /v1/timelines/{timeline_id}`
- `GET /v1/timelines/{id}`
- `PUT /v1/renders/{render_id}`
- `GET /v1/renders/{id}`
- `GET /v1/renders/{id}/receipt`
- `GET /v1/renders/{id}/output`

Errors use a JSON envelope:

```json
{
  "error": {
    "code": "invalid_source_interval",
    "message": "source interval ends beyond the source frame count",
    "field": "spans[0].source_end_frame"
  }
}
```

## Modules

| Module | Responsibility |
|---|---|
| `httpapi` | HTTP routing, request limits, JSON encoding, media responses |
| `service` | Workflow orchestration and job lifecycle |
| `domain` | Entities, validation rules, stable errors, state transitions |
| `audiocodec` | WAV parsing and encoding, resampling, channel mapping, signal metrics |
| `render` | Span evaluation, crossfade mixing, normalization, receipt construction |
| `storage` | Atomic source, timeline, job, output, and receipt persistence |

## Engineering mechanisms

- State transitions: render jobs persist every active and terminal transition.
- Error propagation: domain errors retain stable codes through HTTP responses,
  including a dedicated retryable `unavailable` kind for capacity saturation.
- Resource lifecycle: temporary files and per-render scratch spill files are
  cleaned after success, failure, or abort.
- Concurrency: renders run on a bounded worker pool with per-identifier
  in-flight deduplication; source and timeline operations use per-ID locking
  and lock-free reads, so short operations never wait behind a long render.
- Memory: mixing and encoding stream through a block store that keeps a fixed
  number of resident blocks and spills the rest to scratch storage; an
  admission estimate enforces a configurable total render memory budget.
- Determinism: block pinning preserves the exact float32 operation order, so
  the same timeline renders byte-identical output regardless of spilling or
  parallel scheduling.
- Recovery: on startup, non-terminal jobs are re-adopted; an output folder left
  by a crash between commit and completion is reconciled rather than duplicated.
- Validation boundaries: request size, PCM layout, timeline bounds, and gain
  ranges are checked before allocation-heavy work.

## Validation plan

The runtime manifest exposes one bounded production smoke path per workflow:

- Register a generated WAV, read it back, and confirm a malformed payload fails.
- Compose a two-span timeline, read it back, and confirm an invalid crossfade
  fails.
- Render a crossfaded timeline, download the output, decode it, and match core
  receipt measurements.

These checks use the real HTTP entry points and local storage. They are not a
replacement for the test suite that the later task stage will add.
