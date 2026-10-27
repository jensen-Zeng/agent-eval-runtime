# ADR 0001：v0.1 的执行与信任边界

状态：实施。日期：2026-10-05。

## 决策

1. 采用单个 Go CLI、同步单任务执行，不引入数据库或后台 worker。为后续版本保留 Run/Attempt、Factory/Sandbox、Agent、Evaluator 接口。
2. Windows 控制器连接当前 Docker context 的 Linux Engine，任务只支持 Linux amd64。Docker CLI 只用于读取可信本地 endpoint；任务命令全部通过 Engine SDK 在容器执行。
3. 固定 SDK/Go 版本；准备阶段构建 Go helper。run 解析镜像 ID 并同时用于 Agent/evaluator，避免一次任务中标签变化。
4. 任务工作区与临时目录使用独立 tmpfs，根文件系统只读、不挂宿主目录。默认内存从计划的起始 512 MiB 调为 1024 MiB，以留出 Go 编译空间；实际测试证据在验收记录。tmpfs 提供工作区容量上限，其使用也计入容器内存。
5. 文件工具使用 `os.Root`；源输入和快照拒绝链接/特殊文件。patch 的最终允许文件名单来自可信 TaskSpec。允许命令不等于只允许读 workspace，因此所有秘密和隐藏测试都由控制端隔离。
6. 候选 patch 来自真实快照。另建评测容器，从原始 source + trusted tests 重建后应用 patch；不复用执行环境。
7. 命令超时或取消中止整个专属容器，不尝试复用可能有后台子进程的 workspace。清理使用有限的独立 context。
8. OpenAI 接入采用标准库 HTTP 的 Chat Completions 工具协议，模型可配置、不绑定某个模型别名。HTTP 仅用于本地 mock；真实 provider 使用 HTTPS、不跟随携带 key 的重定向。后续可加 Responses adapter。
9. 使用严格 JSON 代替计划中的 YAML 示例，减少首版依赖；官方 Docker SDK 及其传输依赖仍锁定在 go.mod/go.sum。
10. 文件 artifact 适用于同步单进程版本，不能承担多 worker 的持久队列。进程突然被 kill 的恢复不作为本版保证。

## 后果

可在无真实 LLM key 的情况下，通过确定性动作和本地协议模拟验证平台闭环；真实模型成功率需另行测量。性能与资源数据对应本机 Docker Desktop 的 Linux VM，不能直接代表原生 Linux 性能。Go 编译 cache 位于临时空间，首个任务可能有明显冷启动；增加可信只读预热 cache 时须单独记录镜像版本与实验条件。
