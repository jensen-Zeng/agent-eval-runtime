> v1.3 新增 `runtime harbor inspect` 与 `runtime harbor import`。Harbor 的镜像、文件交付与评分协议见 [接入指南](HARBOR.md)；下文 Go 补丁示例仍适用于 Go 后端。

# CLI 与配置参考

本指南补充 [项目首页](../README.md)。命令均从仓库根目录执行；先完成首页的依赖和镜像准备。版本历史中的本机验收原始记录不随仓库发布。

## 队列服务快速开始：本机队列服务（Windows）

需要 Go 1.27.1、Docker Linux Engine 和本机 PostgreSQL 17。服务与数据库只监听 loopback。用隐藏输入设置数据库密码，密码写入当前进程环境供 Compose 初始化，DSN 仅设置为环境变量、不打印：

```powershell
cd agent-eval-runtime
$secureDbPassword = Read-Host 'PostgreSQL password' -AsSecureString
$plainDbPassword = [System.Net.NetworkCredential]::new('', $secureDbPassword).Password
$env:POSTGRES_PASSWORD = $plainDbPassword
$encodedDbPassword = [uri]::EscapeDataString($plainDbPassword)
$env:AGENTEVAL_DATABASE_URL = "postgres://agenteval:$encodedDbPassword@127.0.0.1:5432/agenteval?sslmode=disable"
docker compose -f deployments/compose.postgres.yml up -d

.\scripts\build.ps1
.\bin\runtime.exe prepare --project .
```

服务固定 2 个 worker，最多排队 32 个任务。API base URL 为 `http://127.0.0.1:8787`；它只接受 loopback 请求并拒绝跨 Origin 请求。
如需提交 LLM 任务，推荐通过网页“连接设置”加密保存并解锁 API Key。也兼容下方的启动进程环境变量方式；已存在加密密钥库时，以密钥库锁定状态为准。CLI 的任务提交请求不接受密钥：

```powershell
$secureKey = Read-Host 'OpenAI API key' -AsSecureString
$env:OPENAI_API_KEY = [System.Net.NetworkCredential]::new('', $secureKey).Password
$env:OPENAI_MODEL = Read-Host 'OpenAI model name'
```

完成准备后，在同一 controller 终端启动服务：

```powershell
.\bin\runtime.exe serve --project . --config configs/local.json --listen 127.0.0.1:8787 --workers 2 --queue-capacity 32
```


## v0.2 API 概览

v0.2 API 位于 `http://127.0.0.1:8787`。常用路由：`GET /healthz`、`GET /readyz`、`GET /metrics`、`GET /v1/tasks`、`POST /v1/runs`、`GET /v1/runs?limit=20&status=QUEUED`、`GET /v1/runs/{job_id}`、`POST /v1/runs/{job_id}/cancel`、`GET /v1/runs/{job_id}/events?after_seq=0&limit=100`。提交必须使用 JSON；`schema_version` 默认为 1，`agent` 默认为 `scripted`，脚本模式的 `script` 默认为 `success`。LLM 模式可省略 `model`，由服务配置提供默认模型；LLM 请求不能带 `script`。可带最多 128 个可打印 ASCII 字符的 `Idempotency-Key`。新 job 返回 202，复用相同 key 与请求返回 200，key 对应不同请求返回 409，队列满返回 429。普通 JSON 响应顶层含 `schema_version: 1` 与 `request_id`，同时设置 `X-Request-ID`；错误含稳定 `code`、`message`、`retryable` 并保留 `error` 字段。Prometheus 文本 metrics 只通过 header 传 request ID。`job_id` 用于 status/events/cancel；只有 job 完成并返回 `run_id` 后，才用 `runtime get <run_id>` 读取报告。

提交示例：

```powershell
$base = 'http://127.0.0.1:8787'
$body = @{ schema_version=1; task_id='go-parser-001'; agent='scripted'; script='success' } | ConvertTo-Json -Compress
$job = Invoke-RestMethod "$base/v1/runs" -Method Post -ContentType 'application/json' -Headers @{ 'Idempotency-Key'='parser-demo-001' } -Body $body
$job | ConvertTo-Json -Depth 10
do {
  Start-Sleep -Seconds 1
  $job = Invoke-RestMethod "$base/v1/runs/$($job.id)"
} while ($job.status -in @('QUEUED','RUNNING'))
Invoke-RestMethod "$base/v1/runs/$($job.id)/events?after_seq=0&limit=100"
Invoke-RestMethod "$base/v1/runs?limit=20"
Invoke-RestMethod "$base/v1/tasks"
Invoke-RestMethod "$base/metrics"
# 如需取消另一项仍在排队或运行的 job，替换为它的队列 ID：
$activeJobId = 'replace-with-active-job-id'
Invoke-RestMethod "$base/v1/runs/$activeJobId/cancel" -Method Post
# job.id 是持久队列 ID；拿到 job.run_id 后，单次运行产物仍用 CLI 查询：
if ($job.run_id) { .\bin\runtime.exe get $job.run_id }
# 在 API client 终端提交 LLM job；模型名放在请求中，绝不发送 API key。
$llmModel = 'replace-with-your-tool-calling-model'
$llmBody = @{ schema_version=1; task_id='go-parser-001'; agent='llm'; model=$llmModel } | ConvertTo-Json -Compress
$llmJob = Invoke-RestMethod "$base/v1/runs" -Method Post -ContentType 'application/json' -Body $llmBody
```

## v0.2 批量运行 demo（需要先启动 serve）

`scripts/demo-v02.ps1` 只连接已经运行的本机 API，提交 20 项 Scripted 样例并等待结果；它不会启动 PostgreSQL、Docker 或 serve。样例包含成功与失败路径，结果需查看逐项状态与 summary。每次新实验请修改 batch name 和幂等键。输出 manifest 会逐项原子更新；Ctrl+C 或 deadline 后服务端 job 继续运行。样例含预期失败 job，因此完整运行可能以 exit code 2 结束；用 manifest 核对逐项结果。

```powershell
.\scripts\demo-v02.ps1
.\scripts\demo-v02.ps1 -BaseUrl http://127.0.0.1:8787 -JobFile examples/v0.2-batch.json -Output artifacts/batches/v02-demo.json -Timeout 10m
```
## v0.1 单次运行：Windows

前置条件：Go 1.27.1、Docker Desktop 的 Linux Engine、Docker CLI。请先安装 Go；Windows 构建脚本也会尝试用户目录的标准安装路径。

```powershell
cd agent-eval-runtime
.\scripts\build.ps1
.\bin\runtime.exe prepare --project .
.\bin\runtime.exe doctor
.\bin\runtime.exe run --task tasks/fixtures/go-parser-001/task.json --agent scripted --script success
```

`prepare` 首次需要联网拉取固定版本的 Go 基础镜像并构建 helper；后续任务/评测容器均禁网。准备不会把宿主源码工作目录、Docker socket、模型密钥或隐藏答案挂进任务容器。每次运行固定使用本地解析的 image ID，记录在报告里。

成功时输出 JSON，`status=SUCCEEDED`、`verdict=success`。结果写入 `artifacts/<run_id>/`；其中的 `artifact_dir` 给出绝对路径。也可以运行 `scripts/demo.ps1` 完成构建、准备与示例运行。

## v0.1 单次运行：Linux

```bash
./scripts/build.sh
./bin/runtime prepare --project .
./bin/runtime doctor
./bin/runtime run --task tasks/fixtures/go-parser-001/task.json --agent scripted --script success
```

脚本未带可执行权限时使用 `bash scripts/build.sh`。控制端支持 Windows/Linux；本版任务环境固定 Linux amd64。需要 Docker Engine 支持 CPU、内存、swap 与 pids 限制，adapter 会检查能力和实际配置。

## 五种确定性路径

```powershell
.\bin\runtime.exe run --script success
.\bin\runtime.exe run --script failure
.\bin\runtime.exe run --script tamper
.\bin\runtime.exe run --script timeout
.\bin\runtime.exe run --script oom
```

| 模式 | 用途 | 预期 |
| --- | --- | --- |
| success | 读代码、修复、执行可见测试 | 独立可信测试通过 |
| failure | 不修复直接结束 | `test_failure` |
| tamper | 通过命令改可见测试，模拟绕过文件工具 | 候选修改违反 allowed_files，`invalid_patch` |
| timeout | 启动带后台子进程的命令 | `TIMED_OUT`，整个专属容器停止 |
| oom | 在受限容器内分配内存 | 资源失败，记录实际 OOM 证据 |

预期失败的 CLI 会返回非零退出码，不能当作脚本/安装损坏。OOM 测试只在受控内存限制下运行；不修改宿主设置。

## OpenAI Agent

默认提供 OpenAI-compatible 接口；模型名称由 `--model`、`configs/local.json` 的 `llm.model` 或 `OPENAI_MODEL` 提供，不固定硬编码模型。单次 CLI 从控制器环境读取密钥；网页服务也支持加密密钥库，详见 [网页指南](WEB_UI.md)。密钥不进入任务。

```powershell
# 隐藏输入，不把 key 写进 PowerShell 命令历史。
$secureKey = Read-Host 'OpenAI API key' -AsSecureString
$env:OPENAI_API_KEY = [System.Net.NetworkCredential]::new('', $secureKey).Password
$env:OPENAI_MODEL = Read-Host '支持工具调用的 OpenAI 模型名称'
.\bin\runtime.exe run --agent llm --config configs/local.json
# 使用后可清除当前终端中的变量。
Remove-Item Env:OPENAI_API_KEY
```

模型须支持 Chat Completions 的 function tools 与 `max_completion_tokens`。接口根据 [OpenAI 官方工具调用文档](https://developers.openai.com/api/docs/guides/function-calling) 实现。兼容服务可以配置 HTTPS `base_url`；只有 loopback 模拟服务允许 HTTP，重定向不会携带 key。`.env` 文件不会自动载入。

本地 httptest 测试覆盖真实协议格式、工具往返、重试、预算和错误；这不是对真实 OpenAI 模型能力的测量。运行真实任务会产生 provider 费用，请在本机配置 key 和预算；单次 provider usage、重试不确定性会进入结果。

## CLI 与配置

```text
runtime version
runtime doctor [--config configs/local.json]
runtime prepare --project . [--config configs/local.json]
runtime serve [--project .] [--config configs/local.json] [--listen 127.0.0.1:8787] [--workers 2] [--queue-capacity 32]
runtime submit --url http://127.0.0.1:8787 --task-id ID [--agent scripted|llm] [--script NAME] [--model MODEL] [--idempotency-key KEY]
runtime status [--url ...] <job_id>
runtime list [--url ...] [--limit 20] [--status QUEUED]
runtime cancel [--url ...] <job_id>
runtime events [--url ...] [--after-seq 0] [--limit 100] <job_id>
runtime tasks [--url ...]
runtime batch --file examples/v0.2-batch.json --output artifacts/batches/demo.json [--url ...] [--wait] [--timeout 10m]
runtime hash --dir tasks/fixtures/go-parser-001/source
runtime run [--task .../task.json] [--project .] [--agent scripted|llm]
            [--script success|failure|tamper|timeout|oom] [--model ...]
            [--config configs/local.json] [--artifacts artifacts]
runtime get [--artifacts artifacts] <run_id>
```

`submit` CLI 成功时打印 `{job, reused}`；HTTP `POST /v1/runs` 仍直接返回 job 对象，字段 `id` 是 queue ID。Batch summary 区分已知 token usage 与 estimated token 统计，unknown 不作为零值。`batch --timeout` 默认为 `10m`，限制所有提交，并在 `--wait` 时同时限制状态轮询。所有 flags 放在位置参数前。配置采用严格 JSON，未知字段会拒绝；`--task` 指向 JSON 文件，source/evaluator 路径相对该文件且须位于 `--project` 指定的可信项目根内。任务 manifest 是可信本地注册材料，不接受公网任意任务定义。

默认命令预算 30 秒、总执行预算 600 秒、准备/评测阶段各最多 120 秒、Agent 最多 20 步、token 上限 30000、每次模型响应最多 4096 输出 token。模型请求单独限时，传输重试计入调用数和保守 token 预算；无法确定的用量标为 unknown。Agent 为收集和评测预留时间；清理有独立的 30 秒 context。资源默认 CPU quota 1、内存 1024 MiB、64 pids、workspace tmpfs 64 MiB；它们是本项目样例配置，不代表所有仓库都能在该预算编译。

本版遇到非法/未知工具参数时会结束当前 attempt 并报告失败，不自动修复参数再重试。普通命令的非零退出码会作为 observation 返回 Agent；超时、取消和 OOM 会中止 attempt。

请求前的 token 预算使用估算值，请求后的统计优先使用 provider usage；估算不能保证实际账单的硬上限，未返回 usage 或重试结果不确定时会明确标记统计质量。

配置由 `--config` 指定；省略时读取内置 `Default()`，不会自动读取 `configs/local.json` 或 `.env`。`serve` 提供服务端 job API；`status/list/cancel/events/tasks/submit/batch` 是本地 CLI HTTP client。队列 `job_id` 与 runner `run_id` 不同：前者用于 status/cancel/events，后者用于 `get`。`batch --wait` 每秒查询，默认超时 10m；等待超时后已有 job 仍由服务继续处理。`run/get` 仍是本地单次 CLI。

`run` 退出码：`0` 成功；`2` 有效任务失败、预算耗尽、超时或取消；`1` 配置/输入/基础设施/存储错误，或清理失败。HTTP client 的 batch 全部成功为 0、任务失败为 2、提交/基础设施/IO/deadline 错误为 1。Ctrl+C 请求取消运行，并使用独立 context 清理。

## 任务与可信评测

本版附带三个独立代码修复任务。可见源码/测试在 `tasks/fixtures/`，可信目标与回归测试单独保存在 `evaluators/`，只进入新的评测容器。

| 任务 | 修复目标 | 注册必需测试数 |
| --- | --- | --- |
| `go-parser-001` | 非负计数解析：空输入、负数、外围空白 | 4 |
| `go-unique-002` | 字符串去重：保留首现顺序、不修改输入、独立结果切片 | 5 |
| `go-range-003` | 整数钳制：上下界、非法区间、负数、包含边界 | 6 |

后两项各提供 `success` / `failure` 脚本，例如：

```powershell
.\bin\runtime.exe run --task tasks/fixtures/go-unique-002/task.json --script success
.\bin\runtime.exe run --task tasks/fixtures/go-range-003/task.json --script failure
```

TaskSpec 校验输入哈希、阶段预算、资源上限、允许修改的源码文件和必需测试。候选补丁来自 sandbox 的实际快照；修改/删除可见测试、增加未注册文件都会拒绝。评测命令固定为 `go test -json -count=1 ./...`，不仅检查 exit code，还要求每个注册测试实际产生 pass 事件，不能凭打印 “PASS” 判成功。

源码/可信测试变化后，先审查，再用 `runtime hash --dir ...` 计算哈希并更新 task revision/manifest；不能为绕过校验自动接受未知内容。目录哈希为 sorted path + NUL + big-endian uint64 长度 + 原始内容，换行变化也会改变哈希。

## 运行证据与安全边界

```text
artifacts/<run_id>/
  result.json / manifest.json / report.md
  attempts/<attempt_id>/
    trace.jsonl
    candidate.patch
    evaluation.json / evaluator.log
```

`result.json` 区分整体 verdict、失败 reason、独立 evaluation 结果和 cleanup 状态；若测试通过但资源清理失败，evaluation 的 success 保留，整体 run 标为 INFRA_ERROR 并返回 1。模型 usage、阶段耗时、两个 sandbox ID、source/evaluator/patch/image 哈希均可核对。轨迹记录有限工具参数/输出；资源统计是容器级累计 CPU 与周期采样内存峰值，短暂峰值可能漏采，采样失败不冒充零。

Docker 容器使用非 root、只读根文件系统、drop ALL capabilities、no-new-privileges、断网与明确限额；可写路径只在容器 tmpfs 内。文件 helper 使用 Go `os.Root` 约束工作区，快照拒绝链接和特殊文件。任意命令可以操作容器内文件，因此敏感宿主文件与 provider key 根本不进入容器。

`/tmp` 显式允许执行，供 Go 编译后运行测试二进制；同时保留 `nosuid,nodev` 和 256 MiB 大小限制。任务 workspace 与临时目录均为随容器销毁的内存文件系统。

Docker 共享内核，平台控制器拥有高权限 daemon 连接。v0.1 单次 CLI 和 v0.2 队列服务都面向可信操作者的本地实验，不是可直接开放公网的恶意多租户平台。v0.2 服务重启时会按持久 OwnerID 清理其遗留容器，并恢复排队任务；运行中断的 job 标记为基础设施失败或已请求取消。[Docker 安全说明](https://docs.docker.com/engine/security/)。

截至 2026-10-05，v0.2 已达到 **WINDOWS_RUNTIME_ACCEPTED**：Windows Go 测试、vet、Docker sandbox/Runner mock 集成及 HTTP 服务验收 A01–A12 全部通过。验收证据、资源汇总与 harness 首轮误判及复验记录见 v0.2 运行验收记录（本机历史记录，不随仓库发布）。Linux 主机运行尚未实测；没有调用真实 OpenAI，也没有真实模型 benchmark。`v0.1` 历史结果保持独立记录。

## 验证与后续计划

```powershell
.\scripts\test.ps1
.\bin\runtime.exe prepare --project .
$env:AGENTEVAL_DOCKER_TESTS = '1'
.\scripts\test.ps1
Remove-Item Env:AGENTEVAL_DOCKER_TESTS
```

Linux 单元/集成测试可分别用 `go test ./...` 和 `AGENTEVAL_DOCKER_TESTS=1 go test ./...`；先运行 prepare。GitHub Actions 配置了 Linux 测试、vet、构建与 Docker 集成；只有实际执行的结果才计作 CI 验收。

v0.1 的历史证据见 v0.1 验收记录（本机历史记录，不随仓库发布）。v0.2 当前实现范围和未验证项目见 v0.2 实现说明（本机历史记录，不随仓库发布）。人工运行验收流程见 v0.2 验收矩阵（本机历史记录，不随仓库发布）。完整版本规划见 [执行计划](EXECUTION_PLAN.md)，设计取舍见 [ADR](decisions/0001-v01-boundaries.md)。
