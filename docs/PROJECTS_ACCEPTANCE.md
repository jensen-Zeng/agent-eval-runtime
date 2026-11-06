# v1.4 项目服务验收记录

本文仅记录已知执行结果，不代替 CI 报告或生产环境验收。待 CI 完成后更新链接与 job 状态。

## 本机检查（2026-11-07）

| 检查 | 结果 | 备注 |
| --- | --- | --- |
| Go 完整测试套件 | 通过 | 显式取消设置 `AGENTEVAL_TEST_DATABASE_URL` 与 `AGENTEVAL_DOCKER_TESTS`，环境依赖的 PostgreSQL 与 Docker 集成项跳过；不代表真实 PostgreSQL 管理集成已验收 |
| `go vet ./...` | 通过 | 仅记录本机执行结果 |
| 网页行为测试 | 4/4 通过 | `node --test scripts/test-webui.cjs`，使用 Node 内置 test/vm，不新增项目依赖 |
| 浏览器视觉检查 | 未完成 | 浏览器自动化工具无法连接，未进行可视化 QA |
| PostgreSQL 17 项目集成 | 待 CI | 本机没有预先存在的 PostgreSQL 17 测试服务；本机未运行此集成测试 |
| Docker 集成 | 未在本机执行 | 本次验收没有运行该集成测试，不将其记为通过 |

## CI 状态

`postgres-integration` 使用 PostgreSQL 17 服务，并以串行模式执行 `Postgres` 和 `Management` 集成测试，避免不同 package 并行争用数据库 advisory lock。当前 CI 结果待运行；本记录不预先标记通过。

## 当前功能边界

认证、项目成员授权、数据集版本、实验幂等提交和每日额度行为由本机 Go 测试与待运行的 PostgreSQL 集成 job 分别覆盖。真实模型质量/费用、浏览器视觉布局，以及公网多租户安全性不属于本记录的已验收范围。服务仍面向绑定 loopback 的可信本机部署。
