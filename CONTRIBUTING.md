# 参与贡献

欢迎提交缺陷报告、文档修订和代码变更。提交前请说明问题的复现步骤、预期行为，以及相关运行产物或日志位置。不要附带 API Key、数据库密码、私有数据集、参考答案或未审查的用户材料。

## 开发环境

- Go 1.27.1。
- Docker Engine 的 Linux 容器能力；集成验证需要 daemon 正在运行。
- Docker Compose；队列服务集成还需要仓库提供的 PostgreSQL 17 容器。

单次 CLI 演示可运行 `scripts/demo.ps1`（Windows）或 `bash scripts/demo.sh`（Linux）；这会准备 sandbox 镜像并执行仓库自带的 Scripted 任务，不需要模型 Key。

## 提交前检查

请按改动范围运行相关检查。Go 代码至少保持 `gofmt` 格式；建议运行：

```bash
go test ./... -timeout 10m
go vet ./...
go build ./cmd/runtime
```

涉及真实 Docker sandbox 生命周期的改动，还应按项目文档准备镜像并运行 Docker 集成检查。不要把未执行的检查描述为已通过。

## 代码与数据边界

- Agent 可见源码与可信 evaluator 分开放置；不要把参考答案、oracle patch 或历史作答放进 Agent 可见题包。
- 新增 fixture 前确认来源、授权和许可；只提交已审查、适合公开的材料。
- 任务源码目前以 Go 为主。新增语言或 evaluator 时，请同时说明运行镜像、可信评分流程和资源限制。
- 变更配置、API、实验 manifest 或报告格式时，更新相应使用文档和示例。
- 文本文件使用 UTF-8；不要提交本机 `artifacts/`、`bin/`、`.setup/` 或密钥配置。

## Pull request

请在 PR 描述中写明变更目的、用户可见影响、执行过的检查及未覆盖的验证。界面或报告有变化时附上脱敏截图或样例。较大范围的架构调整先提交设计说明，便于维护者讨论边界和迁移方式。