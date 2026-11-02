# v1.3 Harbor 运行验收

日期：2026-11-03。版本：AgentEval Runtime 1.3.0。控制端在 Windows 上验收，任务运行在 Docker Linux Engine；Linux amd64 控制端完成交叉构建。

## 已完成的检查

| 检查 | 结果 |
| --- | --- |
| `go test ./... -count=1 -timeout 10m` | 通过，包含原有 Go 任务回归 |
| `go vet ./...` | 通过 |
| Windows / Linux amd64 构建、版本检查 | 通过，版本为 1.3.0 |
| Docker 集成与 Harbor 端到端测试 | 通过；独立镜像、非 root、断网、只读根、资源限制、临时目录、文件权限及清理均有实际容器检查 |
| 自有 `file-artifacts` 演示 | baseline 有效 0 分；脚本正控制 1 分；JSON 与二进制归档及证据导出内容一致 |
| 两份本机 Harbor 题包 | 原镜像构建和导入成功；baseline 为有效 0 分，参考解控制为有效 1 分；候选程序退出 0，清理成功 |
| 图片输入 | 协议模拟服务验证 `image_url` 消息；两份题包的 8 个 GIF 首帧通过实际解码；图像数据不写入轨迹 |

Docker 端到端测试入口为 `TestHarborRuntimeEndToEndArtifactsAndFrozenTask`；低层容器测试入口为 `TestDockerHarborWorkspaceIntegration`。CI 会执行两者。后续检查结果见 [GitHub Actions](https://github.com/jensen-Zeng/agent-eval-runtime/actions/workflows/go.yml)。

## 重复实验与网页/API

验收使用隔离的本机项目副本、临时 PostgreSQL 数据库和独立 artifacts 目录，没有读取用户真实密钥库：

- 网页 API 可列出 Go 与 Harbor 任务，保存 baseline 实验方案。
- 两道 Harbor 任务各重复两次，共 4 个独立 run、8 个独立 sandbox；均为 `score=0, valid=1, classification=capability_result`，基础设施错误为 0。
- `--resume` 复用原 4 个 job，没有新增执行；JSON、CSV 和 4 份证据包导出成功，未遗漏 run。
- 临时 Runtime、PostgreSQL 和测试 sandbox 已清理。

baseline 不修改源码，0 分是预期的有效评测结果。相应命令退出码 2 表示存在有效的任务失败，不代表平台故障。

## 参考解控制

两份题包的原 solution/solve.sh 只生成结果文件，而 grader 还要求提交可重运行的源码。正控制原样复制参考 solver，并使用通用 un.py 接口转发器透传 grader 的命名参数。参考 solver 的 SHA256 未变化，算法与评分器未改。两题均获得 score=1、valid=1，候选进程退出 0；与 baseline 的分差为 1。该验证使用缩短的控制超时，不代表真实模型能力。验证脚本早期的参数与布局错误保留在本机记录中，不作为题目质量结论。

## 实际环境适配

本机两份题包的原 grader 会创建 `/stage_runs`。默认只读根文件系统拒绝此写入，最初正确返回 `valid=0`。通过显式 `--verifier-tmpfs /stage_runs` 配置后，评分容器获得有界临时目录，Agent 容器没有该挂载，原 grader 和评分权重均未修改。

题包原声明为 8 GB 内存。本机验收显式使用 `--memory-mib 2048`；请求值与生效值分别记录。该结果只证明此配置下已验收的流程，不能推断完整模型任务的资源需求，也不表示平台会自动降低资源要求。`storage_mb` 目前仅记录，不作为完整磁盘配额执行。

## 验证边界

没有调用真实付费模型，因此没有模型能力排名或真实模型成本基准。当前兼容范围、媒体工具限制和不支持的 Harbor 配置见 [接入指南](HARBOR.md)。Linux 宿主的完整网页与队列流程仍需独立验收。

私有题包、原始 grader、参考解和日志保留在本机，不随仓库发布。本机运行记录位于被忽略的 `.setup/` 与 `artifacts/`；公开仓库只分发自有演示题。
