# Voice Timeline Renderer

Voice Timeline Renderer is a self-hosted Go service for turning validated PCM
WAV recordings into a crossfaded, peak-normalized mixdown. The service exposes
an idempotent HTTP surface, keeps media and metadata under one data directory,
and records every render transition.

The implementation covers:

- 16-bit integer PCM WAV validation and signal measurement.
- Ordered timeline spans with source-frame selection, gain, and crossfade.
- Linear sample-rate conversion and mono/stereo channel mapping.
- Equal-power neighboring-span crossfades.
- Peak normalization into a requested dBFS target.
- Durable render states, receipts, and atomically committed output folders.
- A bounded streaming renderer that spills mix blocks to scratch storage, so a
  30-minute 48 kHz stereo render completes inside an explicit memory budget.
- At least two concurrent renders through a worker pool, with short source and
  timeline operations never blocked by long renders.
- Explicitly retryable capacity rejection and single-use render identifiers so
  retries never duplicate output or produce contradictory receipts.

Automated tests cover render byte-parity with the original in-memory algorithm,
bounded-memory and concurrent rendering, capacity admission, idempotency, and
crash recovery.

## Requirements

- Go 1.23 or newer.
- A writable local directory for source media, timeline metadata, render jobs,
  output WAV files, and receipts.

No network service, database server, or third-party Go module is required.

## Build

```bash
go build ./...
```

## Run

```bash
go run ./cmd/voiceservice \
  -addr :8093 \
  -data ./var/voice-timeline
```

The health endpoint is `GET http://127.0.0.1:8093/healthz`.

Runtime settings are supplied through arguments:

- `-addr`: HTTP listen address, default `:8093`.
- `-data`: persistent data root, default `./var/voice-timeline`.
- `-max-parallel-renders`: render worker pool size, default `2`.
- `-render-memory-mib`: total render memory budget in mebibytes, default
  `1024`, shared across concurrent renders.
- `-max-output-seconds`: maximum rendered timeline duration, default `1800`
  (30 minutes).

No environment variable is required. Do not put credentials in the repository;
the service does not consume any remote credential.

## Rendering, concurrency, and memory model

Renders execute on a bounded worker pool rather than inline in the HTTP
request. Each render streams its mix through a block store that keeps only a
small set of float32 blocks resident and spills the rest to a private scratch
file under `tmp/`. Crossfade regions are pinned while they still receive
contributions, so the float32 arithmetic order matches a whole-buffer mixer
exactly: the same timeline always renders byte-identical output.

When the worker pool or memory budget is saturated, a new render is persisted in
the `queued` state and the request fails with HTTP `503` and a stable
`render_capacity_exhausted` code plus a `Retry-After` header. No output is
published for a rejected job. Retrying the same `render_id` resumes the queued
job; concurrent or repeated requests for one id are deduplicated, so an output
and its receipt are produced exactly once. A job that genuinely exceeds the
whole memory budget fails terminally with `render_memory_budget_exceeded`.

Source creation and all read operations use per-identifier locking and
lock-free file reads, so they make progress while renders are running.

## HTTP interface

### Register a source

`PUT /v1/sources/{source_id}`

```json
{
  "name": "opening take",
  "wav_base64": "UklGR..."
}
```

The source ID is chosen by the caller and must be 3-64 characters using
letters, digits, underscore, or hyphen. Repeating the same ID with exactly the
same name and bytes is idempotent. Reusing it with different content returns
the `source_conflict` error.

Response:

```json
{
  "id": "opening_take_01",
  "name": "opening take",
  "digest": "b7c0...",
  "byte_count": 96044,
  "sample_rate": 48000,
  "channels": 1,
  "bits_per_sample": 16,
  "frame_count": 48000,
  "duration_ms": 1000,
  "peak_dbfs": -8.0,
  "rms_dbfs": -11.0,
  "created_at": "2026-11-09T12:00:00Z"
}
```

### Compose a timeline

`PUT /v1/timelines/{timeline_id}`

```json
{
  "name": "two part narration",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0
    },
    {
      "source_id": "closing_take_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800
    }
  ]
}
```

Spans are sequential. A later span overlaps its predecessor by
`crossfade_frames`; the overlap is mixed with equal-power weights. A crossfade
must fit within both neighboring spans.

### Start a render

`PUT /v1/renders/{render_id}`

```json
{
  "timeline_id": "two_part_narration_01"
}
```

The render runs through the durable states
`queued -> rendering -> finalizing -> completed`. A failed render records a
terminal `failed` state with a stable error code and does not expose a partial
output folder. When render capacity is saturated the job is durably `queued`
and the response is `503` with `render_capacity_exhausted` and a `Retry-After`
header; resubmitting the same `render_id` is safe and never duplicates output.

### Read resources

- `GET /v1/sources/{source_id}`
- `GET /v1/timelines/{timeline_id}`
- `GET /v1/renders/{render_id}`
- `GET /v1/renders/{render_id}/receipt`
- `GET /v1/renders/{render_id}/output`

The output route returns `audio/wav`. The receipt records source digests, span
placement, channel mapping, measured peak and RMS amplitude, normalization
gain, output digest, and non-fatal warnings.

## Data directory

```text
var/voice-timeline/
  sources/<source_id>/
    source.wav
    metadata.json
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/
    output.wav
    receipt.json
  tmp/
```

Source and render folders are assembled in `tmp/` and moved into place with one
directory rename. Metadata updates use same-directory temporary files followed
by rename.

## Source layout

```text
cmd/voiceservice/       service entry point
cmd/timelinecheck/      workflow execution entry point
internal/httpapi/       routing, request limits, JSON errors, media response
internal/service/       workflow orchestration and idempotency
internal/domain/        entities, validation, errors, and render transitions
internal/audiocodec/    WAV handling, transforms, and signal measurement
internal/render/        timeline preparation, mixing, normalization, receipt
internal/storage/       atomic local repository
internal/checkflow/     bounded production workflow checks
```

## Workflow checks

Run each declared production check from the repository root:

```bash
go run ./cmd/timelinecheck -workflow ingest-audio-source
go run ./cmd/timelinecheck -workflow compose-timeline
go run ./cmd/timelinecheck -workflow render-mixdown
```

Each check creates an isolated temporary data directory, starts the real HTTP
handler, sends generated WAV inputs, and verifies read-back or rendered output.
The checks are bounded and do not require network access.

## Tests

Run the full suite from the repository root:

```bash
go test ./...
```

Notable coverage:

- `internal/render` byte-parity against a reference in-memory implementation,
  including aggressive block spilling, plus a 30-minute bounded-memory render
  and concurrent renders.
- `internal/service` capacity admission, single-use idempotency, parallel
  execution, memory-budget rejection, and crash recovery.
- `internal/httpapi` non-blocking short operations during a render and the
  retryable `503` capacity response.
- `internal/checkflow` end-to-end compatibility and determinism through the
  real HTTP surface.

