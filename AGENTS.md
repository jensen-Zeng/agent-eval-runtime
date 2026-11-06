# AgentEval Runtime project conventions

- Read `README.md` for entry points and `docs/CLI_REFERENCE.md` for CLI details. `docs/DATASETS.md` owns Go dataset import and experiment workflows; `docs/HARBOR.md` owns Harbor T3-RL-v1.0 import guidance.
- Use the Go version declared in `go.mod`. Build with `scripts/build.ps1` on Windows or `bash scripts/build.sh` on Linux.
- Before submitting Go changes, run `go test ./... -count=1 -timeout 10m`, `go vet ./...`, and `go build ./cmd/runtime`. Docker tests require an available Linux engine and explicit `AGENTEVAL_DOCKER_TESTS=1`; distinguish a skipped check from a passing integration check.
- Keep changes small. Test behavior at the owning boundary and preserve the distinction between model/task failures and infrastructure errors.
- Agent code runs in a restricted container; model credentials and database access stay in the controller. The v1.4 service supports authenticated accounts and project-scoped roles/quotas for trusted local collaboration, but is not a public hosted multi-tenant product. Keep it on loopback; site administrators must also be project members to access project resources.
- Publish only the three owned `go-*` demo fixtures and the self-owned `examples/harbor/file-artifacts` Harbor demo. Imported task packages under `tasks/harbor/`, private graders/source materials, `configs/ui/`, `.setup/`, and `artifacts/` remain local and must not be committed.
- Text files use UTF-8. Preserve existing BOMs; use explicit UTF-8 reads in PowerShell. Task content hashes include bytes and newlines, so do not mechanically reformat registered fixture source or graders.
