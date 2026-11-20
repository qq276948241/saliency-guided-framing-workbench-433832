# Saliency crop orchestration layer

This document describes the three modules added on top of the existing
`smartcrop` API:

| Module | Files | Purpose |
| --- | --- | --- |
| Input normalization | `input.go`, `errors.go` | Validate requests, enforce budgets, copy pixels into an immutable canonical image |
| Candidate scoring | `scoring.go`, `saliency.go`, `digest.go`, `cache.go` | Deterministic candidate generation, parallel scoring, ranking, caching |
| Result acceptance | `acceptance.go`, `cmd/smartcrop-acceptance` | Offline samples and assertions that prove the invariants |

The legacy `SmartCrop`/`Analyzer`/`Options` behavior is preserved. `Analyzer`
keeps its exported surface (`NewAnalyzer`, `NewAnalyzerWithLogger`,
`FindBestCrop`, `options.Resizer`, `Logger`, `Score`, `Crop`), and its scoring
math now routes through the same parameterized kernel with
`legacyScoringParams`, which is numerically identical to the previous
implementation (`TestLegacyHelpersMatchParameterizedKernel` pins this).

## Input normalization

`Normalize(req Request, limits Limits) (*NormalizedImage, error)` returns an
owned, canonical `image.RGBA` with origin `(0,0)`.

* Accepts `*image.Gray`, `*image.RGBA`, `*image.NRGBA`, `*image.Paletted`,
  sub-images and arbitrary `image.Image` implementations with non-zero bounds.
* Alpha is premultiplied during the copy (same conversion as `Analyzer`).
* A single zero requested dimension falls back to the source dimension on that
  axis; both zero is `ErrInvalidDimensions`.
* The fitted target is the largest box with the requested aspect ratio that
  fits the image (integer rounded half-up), so it always has at least 1px on
  both axes — this also covers 1x1 and thin images.
* Rejects, with stable sentinels:

| Condition | Error |
| --- | --- |
| nil image | `ErrNilImage` |
| zero-area or inverted bounds | `ErrEmptyImage` |
| `Bounds()` panics, `At()` panics, allocation panics | `ErrUnreadablePixels` / `ErrDimensionOverflow` |
| pixel count or dimensions overflow | `ErrDimensionOverflow` |
| pixel budget exceeded | `ErrBudgetExhausted` |
| negative pixel/metadata budget | `ErrNegativeBudget` |
| metadata > limit or invalid UTF-8 | `ErrInvalidMetadata` |
| caller context canceled / timed out | `ErrCanceled` / `ErrDeadlineExceeded` |

The copy runs in 64-row stripes and checks the context between stripes, so
cancellation during a multi-megapixel copy returns promptly and the
half-copied `image.RGBA` never escapes the function.

## Candidate scoring

`NewScorer(cfg Config)` + `(*Scorer).Rank(ctx, *NormalizedImage)` produce a
`Ranking`; `NewOrchestrator(cfg)` wraps normalization, scoring and caching.

Candidate generation rules, in order:

1. Scales are enumerated in exact tenths from `MaxScale` down to `MinScale`
   (default `1.0`, `0.9`), matching the legacy grid.
2. Candidate size is `roundHalfUp(target * scale)`, clamped to `[1, srcSize]`.
3. Grid positions advance by `Step` (default 8) from `0` to `max-min`.
4. With jitter enabled, each grid point is offset by a deterministic value in
   `[0, Step)` derived from `seed`, the scale index and the grid coordinates —
   never from iteration order.
5. Every rectangle is `Intersect`ed with the image bounds; empty results and
   duplicates are dropped; a candidate rectangle therefore never escapes the
   image and is never empty.
6. Generation stops with `ErrBudgetExhausted` (no partial ranking) once
   `MaxCandidates` would be exceeded.

Scoring uses the legacy importance function with configurable knobs:

* `DisableRuleOfThirds` removes the thirds boost only.
* `EdgeWeight` replaces the `-20` edge-band penalty (`edgeRadius = 0.4`).
* Scores are always `detail*0.2 + skin*1.8 + saturation*0.3` divided by the
  crop area, exactly as before.

Ranking is a total order: `TotalScore desc`, then `Min.Y`, `Min.X`, `Max.X`,
`Max.Y` ascending, then generation `Order` ascending. Non-finite scores rank
last so a NaN can never win.

Parallelism: workers pull candidate indices from an atomic counter and write
to distinct slots in a preallocated slice. The saliency map is built once,
read-only, and shared. `PeakWorkers`/`ActiveWorkers` are reported through
`Stats`; `ActiveWorkers()` is zero after every return path. `Concurrency=1`
and `Concurrency=N` produce identical rankings (`TestScorerSerialEqualsParallel`).

`Config` also provides:

* `Seed` — fixed seed ⇒ identical candidate set and winner.
* `Timeout` — combined with the caller context.
* `CacheSize` — bounded LRU in `Orchestrator`; `-1` disables it. Only
  successful results are cached; canceled, timed-out, failed and
  budget-exhausted runs never write to it.
* `TopCandidates` — caps `Result.Top` while `Ranking.Candidates` keeps all.

Every successful result carries `Digest` (pixels + request + config +
algorithm version) and `ImageDigest` (pixels only). `Result.Summary()` is a
one-line stable string that the acceptance command compares across runs.

### Determinism coverage

`scoring_test.go` / `orchestrator_test.go` assert identical rectangles and
scores across repeated runs and seeds for: transparent-edged NRGBA images,
solid-color images, thin tall images, non-ASCII metadata, non-zero bounds,
1x1 tiny images, and a 2048×2048 image under a large candidate budget.

## Result acceptance

`RunAcceptance(ctx, AcceptanceOptions)` executes the offline suite and returns
a JSON-serializable `AcceptanceReport`:

* Legality of every selected rectangle (inside the canonical image, correct
  aspect within rounding, translatable back to source coordinates).
* Ranking order of `Top` and `Stats.Scored == Stats.Candidates`.
* Digest stability across two runs of every sample.
* Parallel-equals-serial for both the winner and the full candidate list.
* Cancellation and timeout: stable sentinel errors, zero `Result`, zero active
  workers, zero cache entries.
* Corrupt images (panicking `Bounds`/`At`), nil images, and budget exhaustion
  all fail atomically.
* Optional legacy pin: `LegacyExpectation` re-runs `Analyzer.FindBestCrop` and
  asserts the rectangle is unchanged.

The report includes a `limitations` list recording conditions the environment
cannot cover (EXIF/ICC decoding, multi-image legacy pinning, wall-clock
performance), so gaps are explicit rather than skipped.

### Command

```sh
go run ./cmd/smartcrop-acceptance -seed 1 -repeat 2 -out /tmp/smartcrop-acceptance.json
```

It runs the suite `-repeat` times, fails if the summaries differ, prints the
JSON report, and exits non-zero if any check fails.

### Test coverage of the hard cases

| Requirement | Test |
| --- | --- |
| Concurrency/races | `TestScorerConcurrentRankSameImage`, `TestOrchestratorConcurrentUse`, `go test -race ./...` |
| Repeated submission | `TestOrchestratorRepeatedSubmissionSameSummary`, `TestSummaryStableAcrossRuns` |
| Corrupt images | `TestNormalizeUnreadablePixels`, `TestPanickingImagesAreStableAcrossEntryPoints` |
| Budget exhaustion | `TestScorerBudgetExhausted`, `TestOrchestratorBudgetExhaustedLeavesNoState` |
| Two-run summary comparison | `TestRunAcceptanceRepeatable`, `cmd/smartcrop-acceptance -repeat 2` |
| Legacy API unchanged | `TestCrop` (existing), `TestLegacyAnalyzerUnchanged`, `TestLegacyHelpersMatchParameterizedKernel` |

Failure diagnostics: every error is an `*OpError` exposing `Op`, the sentinel
via `Unwrap`, and a `Detail` string with concrete values (dimensions, budgets,
rectangles). Cancellation sentinels also match `context.Canceled` /
`context.DeadlineExceeded` through `errors.Is`.
