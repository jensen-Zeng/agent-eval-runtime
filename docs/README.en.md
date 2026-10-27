# AgentEval Runtime

[中文 README](../README.md)

A Go-based local platform for running code-repair agents in restricted Docker containers and evaluating their changes in separate trusted evaluator containers.

Version 1.2.0 includes a CLI, a PostgreSQL-backed job queue, a local HTTP API, and an embedded Chinese web console. The current task runtime supports Go code-repair tasks in Linux amd64 containers. Windows and Linux use the same controller implementation.

## Features

- Scripted demos and an OpenAI-compatible tool-calling agent.
- Time, process, CPU, memory, file, output, and token budgets.
- Independent grading with registered Go tests or a trusted Python grader.
- Frozen task × model × repetition experiments, resume, and JSON/CSV summaries.
- Persistent jobs, cancellation, leases, heartbeats, fencing, and container reconciliation.
- Execution traces, candidate patches, sampled resource usage, input hashes, and evidence exports.
- Local experiment presets and an encrypted API-key vault that locks after restart.

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

For the queue and web console, run `scripts/start.ps1` on Windows or `bash scripts/start.sh` on Linux. The script builds the runtime, starts PostgreSQL through Compose, prepares the sandbox image, and starts the controller. Open <http://127.0.0.1:8787/>. Reuse the same database password for an existing volume.

After the initial image preparation, Windows users can also use `打开控制台.cmd`. Model experiments require a configured provider and may incur charges.

## Boundaries

This is a trusted-operator, local single-user platform. Containers share the Linux kernel; the Docker daemon, registered graders, and images are trusted. It is not a public multi-tenant service. Real paid-model benchmarks, multi-host workers, and general multi-language task execution are not included in the current validation scope.

Only three owned Go demo tasks are distributed. Imported assignment packages, private graders, credentials, and execution artifacts remain local. See [third-party notices](../THIRD_PARTY_NOTICES.md).

See the [main README](../README.md) for architecture, model setup, experiments, and documentation links; [CONTRIBUTING](../CONTRIBUTING.md) for development checks; and [SECURITY](../SECURITY.md) for the security boundary and vulnerability reporting.

Licensed under the [MIT License](../LICENSE).
