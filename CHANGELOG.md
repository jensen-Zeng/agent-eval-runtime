# Changelog

## 1.2.0

- Added an embedded local web console for task selection, model/repeat configuration, saved experiment presets, run history, traces, and evidence exports.
- Added an encrypted local API-key vault; the master password is not persisted and the vault is locked after restart.
- Retained the same task, experiment, scheduler, and evaluation contracts for CLI and web workflows.

## 1.1.0

- Added generic dataset manifests, an optional E3 directory adapter, reviewed batch imports, and offline dependency-image preparation.
- Added dataset experiment matrices and submission-evidence exports.

## 1.0.0

- Added frozen task/model/repetition experiments, input and image hashes, resume, and independent patch re-evaluation.
- Added trusted Python graders with partial scores alongside registered Go tests.
- Added database leases, heartbeats, fencing, and owner-scoped container reconciliation.

## 0.2.0

- Added a PostgreSQL-backed job queue, bounded worker pool, local HTTP API, cancellation, and recovery.

## 0.1.0

- Added single-run Go task execution, a restricted Docker backend, scripted and OpenAI-compatible agents, independent evaluation, and execution traces.

## Repository publication — 2026-10-27

- Adopted `github.com/jensen-Zeng/agent-eval-runtime` as the module path.
- Added a GitHub-facing README, contribution and security guidance, MIT license, and Windows/Linux CI.
- Kept imported assignment packages, local credentials, and historical execution artifacts outside the published repository. This packaging update does not imply a new model-capability benchmark.
