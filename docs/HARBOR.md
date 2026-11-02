# Harbor 题包接入（v1.3）

本指南介绍 AgentEval Runtime 对 Harbor `T3-RL-v1.0` 题包的本机接入子集。该适配保留已有 Go 任务后端；两种任务可进入同一任务清单、网页实验和队列流程。它不是对所有 Harbor 版本或任意容器任务的通用运行器。

## 支持范围

Runtime 接受以下 Harbor 结构和运行约定：

- `version = "T3-RL-v1.0"`，Agent 工作目录为 `/app`，`environment/upload` 从 `environment/workspace` 映射到 `/app`。
- 任务容器运行时断网；`verifier.environment_mode = "separate"`，判分器在独立容器中运行。
- 评分类型为 `judge.type = "programmatic"`、`reward_kind = "continuous"`。`tests/test.sh` 调用 `other.grader`，并写入 `/logs/verifier/reward.json`；原 grader 保持不变，由其 Harbor 适配层输出 reward/valid。
- 每个题包提供 `environment/Dockerfile` 和 `tests/Dockerfile`。预构建镜像专用包（只写 `docker_image`）不支持。
- `other.required_outputs` 中的路径相对于 `/app`。Runtime 在提交归档中保留 Agent 工作区快照，并将其交给独立 verifier。

当前不接受共享 verifier、网络运行模式、需要 LLM judge/secret 的配置、仅预构建镜像包或其他 Harbor schema。导入器会检查这些配置以及路径、文件和镜像构建入口；不支持的包应在 `inspect` 阶段停止。

## 检查题包

从仓库根目录运行。路径使用你自己的本地题包目录，不要把私有材料路径写进文档或版本库：

```powershell
.\bin\runtime.exe harbor inspect --path YOUR_HARBOR_TASK
```

`inspect` 只读题包，不构建镜像，也不注册任务。兼容包会显示格式版本、文件数量和大小、超时、内存请求、未强制执行的 storage 请求及必需产物。`review_warnings` 只是凭据样式内容的人工复核提示；导入前请检查并移除真实凭据。导入器不会把原始题包上传到远端。

仓库自带的 `examples/harbor/file-artifacts` 是可公开使用的结构演示题包。它已经通过真实 Docker 端到端验收：无产物 baseline 为有效 0 分，文件正控制为 1 分。

## 本机导入

先构建 Runtime，并确保 Docker Linux Engine 正常运行。导入会按本机可信的 Dockerfile 构建 Agent 和 verifier 专用镜像，给基础镜像加入 Runtime helper，生成两侧最终镜像，并记录不可变的 Docker image ID、题包 SHA256、workspace SHA256 和 tests SHA256。`PackageSHA256` 覆盖导入副本内的 `task.toml`、`instruction.md`、`environment/`、`tests/` 和 `solution/`；solutions 留在本地导入副本，不会进入 Agent 或 verifier 镜像。

```powershell
.\scripts\build.ps1
.\bin\runtime.exe harbor import --path YOUR_HARBOR_TASK --project . --config configs/local.json --memory-mib 2048 --verifier-tmpfs /stage_runs
```

`--id` 可覆盖 `metadata.instance_id`；默认使用该字段。`--memory-mib` 是显式的有效内存限额，允许 1024–16384 MiB；省略时使用题包原始 `environment.memory_mb`，不会静默降低。示例的 2048 MiB 是针对本机 Docker 仅分配 8 GiB RAM 的显式覆盖值，题包原始值仍保存在元数据中。若你在其他环境运行，应按实际 Engine 内存和任务需求选择，而不是照抄该值。

`--verifier-tmpfs` 可选地为 verifier 指定最多 4 个顶层 scratch 目录，逗号分隔，例如 `/stage_runs,/scratch2`。这项覆盖仅给 verifier 容器增加有界 tmpfs（每个目录最多 1024 MiB、`noexec`）；Agent 容器和只读 rootfs 权限不变。默认不额外挂载目录。仅当已审核的 grader 确实要在 rootfs 下写临时状态时使用；不能覆盖 `/app`、`/logs`、`/tmp` 等系统路径，也不改变 verifier 网络、用户或 capability 限制。该列表写入 task manifest 并随实验快照冻结。

导入器整体读取上限为 128 MiB、10000 个文件；Agent workspace 与 verifier tests 各自最多 4000 个文件、单文件 16 MiB、总计 64 MiB。超限会在导入时拒绝。

导入目录位于 `tasks/harbor/<task-id>/`，同 ID 已存在时拒绝覆盖。导入成功后重启 Runtime 服务，使启动时的任务 registry 加载新任务；网页本版没有原始题包导入按钮。题包的 Dockerfile、grader、依赖和参考解均视为可信的本地执行输入；镜像构建可执行题包中的构建指令，请只导入审核过的材料。

## 资源预算与 Docker 容量

Harbor 的 Agent 与 verifier 超时按 `task.toml` 注册：当前上限分别为 86400 秒与 3600 秒；总运行预算由 Agent 时间、verifier 时间、收集和清理时间构成。Agent 命令上限 300 秒。请求 CPU 上限 4，内存上限 16384 MiB。工作区使用有界的 1024 MiB tmpfs，临时目录上限 1024 MiB。题包 `storage_mb` 只记录为元数据，不会变成硬盘容量配额；不要据此假设获得 16 GiB 可用磁盘空间。

服务启动时按 `worker 数 × 已登记任务中的最大内存限额` 预留 Engine 内存，要求不超过 Docker Engine 报告 RAM 的 75%。例如 Docker Engine 可用内存为 8 GiB 时，两个 worker 配合 8192 MiB 的 Harbor 任务不满足该门槛；显式导入为 2048 MiB 后，仍须根据所有任务和 worker 总数重新核对。遇到预留超限时，应降低 worker 数或通过受控资源配置重新导入任务。已经导入的 task manifest 不应手工编辑绕过预算校验；修改有效资源配置应使用经审核的新任务副本/ID。

## 实验、结果和证据

服务重启并加载任务后，在网页实验台选择已登记的 Harbor 任务。Harbor 任务可与 Go 任务混合选择；网页可组合基线、多个模型和重复次数，并保存实验方案。仓库自带 `examples/harbor/file-artifacts` 可作为无 Key 的自有格式演示：

```powershell
.\bin\runtime.exe harbor inspect --path examples/harbor/file-artifacts
.\bin\runtime.exe harbor import --path examples/harbor/file-artifacts --project . --config configs/local.json --verifier-tmpfs /stage_runs
```

重启服务后，在网页只选 baseline/no-op 即可走不调用模型的流程。该流程没有模型费用；镜像构建仍可能访问 Dockerfile 指定的软件源。真实模型实验需解锁已配置的 provider，可能产生费用；本版本没有宣称已完成真实付费模型能力评测。

Agent 的最终 `/app` 工作区会导出为 `submission.tar.gz`。verifier 必须输出 JSON，例如：

```json
{"reward": 0.85, "valid": 1.0}
```

`valid = 1` 表示评分结果有效，即使 reward 为 0 也作为有效的模型结果记录；`valid = 0` 表示 grader/环境结果不可信，归类为基础设施错误，而不是模型得 0 分。reward 归一化范围是 0–1；目前通用 Harbor 通过阈值固定为 1.0，所以 partial reward 会保留在 score 中，但 verdict 仍是 `test_failure`。运行报告和导出 bundle 包含 `submission.tar.gz`、评分 JSON、轨迹与 manifest 中的输入/镜像哈希，具体文件以报告 `artifacts` 字段为准。

命令行实验和导出操作见 [CLI 参考](CLI_REFERENCE.md)；队列服务、网页方案及运行记录见 [网页控制台指南](WEB_UI.md)。

## 图片、视频与音频输入

Harbor 使用 `write_file` 或 `exec` 修改文件，不依赖镜像内安装 Git，因此不提供 Go 后端的 `apply_patch` 工具。`read_image` 可把 PNG、JPEG 或 GIF 图像呈现给模型；GIF 可请求第 0–31 帧。超出该范围的动画帧可通过容器内 `exec` 调用已安装工具提取，再使用 `read_image` 查看。其他图片格式、视频和音频没有原生多模态消息接口；若镜像中有相应工具，可由 Agent 使用 `exec` 做帧提取或媒体处理，再把生成的文本/图像输入交给模型。能够运行媒体处理命令不等于模型直接看过视频或听过音频。

需要选择支持图像输入的模型。图像 token 使用固定估算预算，最终用量以 provider usage 为准，估算不保证实际账单硬上限。

模型请求会把题面以及 Agent 通过工具读取并放进对话的内容发送给所配置的模型服务。题包导入与镜像构建在本机进行，不会上传原始包或私有 verifier；仍应按数据敏感级别选择是否使用外部模型服务。

## 当前验收状态

已通过全量 Go 回归、静态检查、Windows/Linux 构建和真实 Docker 集成验收。两份本机 Harbor 题包已完成原 grader baseline、参考解正控制与四项重复实验，网页/API 的保存方案、恢复、JSON/CSV 和证据导出通过。完整口径见 [v1.3 运行验收](HARBOR_ACCEPTANCE.md)。没有调用真实付费模型，运行验收不代表模型能力结论。
