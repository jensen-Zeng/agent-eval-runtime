# Changelog

## 1.4.0

- Added first-run administrator setup, authenticated sessions, user management, and project membership with owner/executor/reader roles.
- Added immutable project dataset versions and project-scoped experiment submission, jobs, events, cancellation, and usage.
- Added transactionally reserved daily task quotas, running concurrency limits, and idempotent experiment submission.
- Closed legacy unscoped `/v1/runs` GET and write paths to prevent bypassing project authorization and accounting; direct trusted local `runtime run` remains available outside service quotas.
- Kept the service intended for trusted local deployments; account and project controls do not make it a public multi-tenant product.

## 1.3.0

- Added a constrained Harbor T3-RL-v1.0 task backend with separate local agent/verifier image builds, frozen package/image IDs, programmatic continuous reward parsing, and submission archive export.
- Added a Harbor package inspection/import CLI and a self-owned file-artifacts demo fixture; raw Harbor import remains a CLI workflow followed by a service restart.
- Added Harbor image viewing for PNG/JPEG and GIF frames 0–31; video/audio remain in-container `exec` workflows without native model message inputs.
- Documentation records unsupported Harbor modes and the distinction between static package compatibility and Docker/model end-to-end validation.

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
