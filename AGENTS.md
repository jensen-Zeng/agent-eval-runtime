# AgentEval Runtime project conventions

- Read `README.md` for entry points and `docs/CLI_REFERENCE.md` for CLI details. `docs/DATASETS.md` owns dataset import and experiment workflows.
- Use the Go version declared in `go.mod`. Build with `scripts/build.ps1` on Windows or `bash scripts/build.sh` on Linux.
- Before submitting Go changes, run `go test ./... -count=1 -timeout 10m`, `go vet ./...`, and `go build ./cmd/runtime`. Docker tests require an available Linux engine and explicit `AGENTEVAL_DOCKER_TESTS=1`; distinguish a skipped check from a passing integration check.
- Keep changes small. Test behavior at the owning boundary and preserve the distinction between model/task failures and infrastructure errors.
- Agent code runs in a restricted container; model credentials and database access stay in the controller. Do not expose the current local single-user API as a public multi-tenant service.
- Publish only the three owned `go-*` demo fixtures. Imported task packages, private graders, `configs/ui/`, `.setup/`, and `artifacts/` remain local and must not be committed.
- Text files use UTF-8. Preserve existing BOMs; use explicit UTF-8 reads in PowerShell. Task content hashes include bytes and newlines, so do not mechanically reformat registered fixture source or graders.
