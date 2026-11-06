# AgentEval Runtime

[中文 README](../README.md)

A Go-based local platform for running code-repair agents in restricted Docker containers and evaluating their changes in separate trusted evaluator containers.

Version 1.4.0 includes a CLI, a PostgreSQL-backed job queue, an authenticated project-scoped HTTP API, and an embedded Chinese web console with account and project management. Its task backends include Go code-repair tasks and a constrained Harbor `T3-RL-v1.0` subset running in Linux amd64 containers. Windows and Linux use the same controller implementation.

## Features

- Scripted demos and an OpenAI-compatible tool-calling agent.
- Time, process, CPU, memory, file, output, and token budgets.
- Independent grading with registered Go tests or a trusted Python grader.
- Local CLI import of Harbor packages using separate agent and verifier images, `/app` workspaces, and programmatic continuous rewards. Restart the service after import; the web console selects registered tasks and does not upload/import raw Harbor packages.
- Frozen task × model × repetition experiments, resume, and JSON/CSV summaries.
- Persistent jobs, cancellation, leases, heartbeats, fencing, and container reconciliation.
- Execution traces, candidate patches, sampled resource usage, input hashes, and evidence exports.
- Project-scoped users and roles, immutable dataset versions, idempotent experiment groups, daily usage accounting, and per-project concurrency limits.
- An encrypted API-key vault that locks after restart; model credentials remain controller-side.

## Quick start

Install the Go version in `go.mod` and start a Docker Linux engine. Clone the repository:

```bash
git clone https://github.com/jensen-Zeng/agent-eval-runtime.git
cd agent-eval-runtime
```

Run the bundled scripted demo without a model API key or PostgreSQL:

```powershell
# Windows
.\scripts\demo.ps1
```

```bash
# Linux
bash scripts/demo.sh
```

For the local service and web console, run `scripts/start.ps1` on Windows or `bash scripts/start.sh` on Linux. The script builds the runtime, starts PostgreSQL through Compose, prepares the sandbox image, and starts the controller. Open <http://127.0.0.1:8787/> and create the initial administrator account on first use. Create a project, add its members, then create a dataset version and submit experiments. See the [project management guide](PROJECTS.md). Reuse the same database password for an existing volume.

After the initial image preparation, Windows users can also use `打开控制台.cmd`. Model experiments require a configured provider and may incur charges.

## Boundaries

The service supports local accounts and project-scoped roles for collaboration by trusted operators. Every project resource requires explicit membership, including for site administrators. Containers share the Linux kernel; the Docker daemon, registered graders, and images are trusted. This remains a local deployment product, not a public hosted multi-tenant service. Harbor support is limited to the documented T3-RL-v1.0 subset; shared verifiers, network-enabled tasks, LLM judges, prebuilt-image-only packages, and other Harbor schemas are unsupported. Real paid-model capability runs, multi-host workers, and general multi-language task execution are not included in the current validation scope.

Harbor task packages can include images read through the agent's PNG/JPEG/GIF tool (GIF frames 0–31). The model API accepts text and tool results; video and audio are not native message inputs and require in-container processing through `exec`. See the [Harbor guide](HARBOR.md) for import, resource, scoring, and evidence details.

Only three owned Go demo tasks are distributed. Imported assignment packages, private graders, credentials, and execution artifacts remain local. See [third-party notices](../THIRD_PARTY_NOTICES.md).

See the [main README](../README.md) for architecture, model setup, experiments, and documentation links; the [project management guide](PROJECTS.md) covers authentication, authorization, datasets, idempotency, and quotas; [CONTRIBUTING](../CONTRIBUTING.md) for development checks; and [SECURITY](../SECURITY.md) for the security boundary and vulnerability reporting.

Licensed under the [MIT License](../LICENSE).
