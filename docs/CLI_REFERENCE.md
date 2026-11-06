> v1.3 新增 `runtime harbor inspect` 与 `runtime harbor import`。v1.4 增加项目服务与认证；见[项目管理指南](PROJECTS.md)。Harbor 的镜像、文件交付与评分协议见 [接入指南](HARBOR.md)；下文 Go 补丁示例仍适用于 Go 后端。

# CLI 与配置参考

本指南补充 [项目首页](../README.md)。命令均从仓库根目录执行；先完成首页的依赖和镜像准备。版本历史中的本机验收原始记录不随仓库发布。

## v1.4 本机服务

队列服务使用 PostgreSQL 17 并默认绑定 loopback。推荐通过仓库启动脚本初始化数据库和 sandbox 镜像：

```powershell
.\scripts\start.ps1 -ConfigFile configs/local.json
```

```bash
bash scripts/start.sh
```

首次启动时设置 PostgreSQL 密码并打开 `http://127.0.0.1:8787/`，创建初始管理员账号。之后在网页创建项目、加入成员、授权数据集版本并提交实验。账号和项目角色不改变平台面向可信本机部署的安全边界。认证、数据集版本、角色和配额规则见[项目管理指南](PROJECTS.md)。

服务的管理 HTTP 路由位于 `/v1/manage`，项目任务必须使用 `/v1/manage/projects/{project_id}/...` 路径。旧的无项目 `/v1/runs` 路由（GET 和写请求）都已关闭；`runtime submit`、`status`、`list`、`cancel`、`events`、`tasks`、`batch`、`experiment` 都是旧 HTTP client 命令，不能用于 v1.4 项目服务。不要把旧命令作为当前服务工作流。

真实模型任务需从网页连接设置配置 OpenAI-compatible endpoint、模型和加密 API Key。Key 留在控制端，不发送给 Agent 容器。模型服务会收费；本机直接 `runtime run` 是可信管理命令，不经过项目服务额度；演示由 `scripts/demo.ps1` 或 `scripts/demo.sh` 启动，不存在 `runtime demo` 子命令。

## CLI 与配置

```text
runtime version
runtime doctor [--config configs/local.json]
runtime prepare --project . [--config configs/local.json]
runtime serve [--project .] [--config configs/local.json] [--listen 127.0.0.1:8787] [--workers 2] [--queue-capacity 32]
runtime hash --dir tasks/fixtures/go-parser-001/source
runtime run [--task .../task.json] [--project .] [--agent scripted|llm]
            [--script success|failure|tamper|timeout|oom] [--model ...]
            [--config configs/local.json] [--artifacts artifacts]
runtime get [--artifacts artifacts] <run_id>
runtime dataset ...
runtime harbor inspect|import ...
```

`run`、`get`、dataset 导入/检查和 Harbor 导入命令用于可信本机管理工作流。`runtime run` 不占项目服务配额；演示由 `scripts/demo.ps1` 或 `scripts/demo.sh` 启动。`runtime experiment` 是旧的 HTTP client 命令，会访问已关闭的无项目 `/v1/runs` API；`submit`、`status`、`list`、`cancel`、`events`、`tasks`、`batch` 同样不能作为 v1.4 管理 API 客户端。项目服务实验请登录网页并选择项目和数据集版本。

配置采用严格 JSON，未知字段会拒绝；省略 `--config` 时读取内置 `Default()`，不会自动载入 `configs/local.json` 或 `.env`。`--task` 指向可信本地任务 JSON；source/evaluator 路径相对任务文件，并须位于 `--project` 指定的项目根目录内。服务端由已登记任务目录和项目数据集版本选择任务，不接受 HTTP 任意任务定义。

默认单次任务命令预算 30 秒、总执行预算 600 秒、准备/评测阶段各最多 120 秒、Agent 最多 20 步、token 上限 30000、每次模型响应最多 4096 输出 token。模型请求单独限时，传输重试计入调用数和保守 token 预算；无法确定的用量标为 unknown。Agent 会为评测和清理预留时间；清理有独立的 30 秒 context。资源默认 CPU quota 1、内存 1024 MiB、64 pids、workspace tmpfs 64 MiB；它们是仓库示例预算，不代表所有项目都能在该预算内编译。

`run` 的退出码：`0` 成功；`2` 有效任务失败、预算耗尽、超时或取消；`1` 配置、输入、基础设施、存储错误或清理失败。普通命令的非零退出码会作为 observation 返回 Agent；超时、取消和 OOM 会中止 attempt。Ctrl+C 请求取消运行，并使用独立 context 清理。

请求前的 token 预算使用估算值，请求后的统计优先使用 provider usage；估算不能保证实际账单的硬上限，未返回 usage 或重试结果不确定时会明确标记统计质量。真实模型调用可能产生服务商费用。
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
