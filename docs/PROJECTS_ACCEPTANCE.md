# v1.4 项目服务验收记录

本文记录 AgentEval Runtime 1.4.0 在代码提交 `e2606dae92e9d9d17c678cd874a8bb6ce498cdfb` 上的本机与 CI 验收。后续仅文档的提交不会改变这份结果所对应的运行代码。

## 本机检查（2026-11-07）

| 检查 | 结果 | 范围 |
| --- | --- | --- |
| `go test ./...` | 通过 | 本机完整 Go 测试套件；没有配置 `AGENTEVAL_TEST_DATABASE_URL`，所以需真实 PostgreSQL 的测试由 CI 的 PG17 job 覆盖 |
| `go vet ./...` | 通过 | 本机检查 |
| `go build -o bin/runtime.exe ./cmd/runtime` + `runtime version` | 通过，版本 1.4.0 | Windows 本机 CLI |
| `node --test scripts/test-webui.cjs` | 4/4 通过 | 使用 Node 内置 test/vm，没有新增 npm 依赖 |
| PostgreSQL 17 管理集成 | 未在本机运行 | 本机没有现成的 PostgreSQL 17 服务；真实数据库验收见下方 CI job |
| Docker 集成 | 未在本机运行 | 本机没有可用于本次验收的现成 Docker 测试组件；通过 CI 的 Linux Docker integration job 验收 |
| 浏览器视觉检查 | 未完成 | 浏览器视觉 QA 未执行，不将 Node 行为测试计作视觉检查 |

本次没有新增 Go 或 npm 依赖，也没有调用真实模型服务。

## GitHub Actions 验收

代码提交对应的 [GitHub Actions run 42249334446](https://github.com/jensen-Zeng/agent-eval-runtime/actions/runs/42249334446) 已完成，四个 job 均为 success：

| Job | 结果 | 验收范围 |
| --- | --- | --- |
| Ubuntu Unit/build | success | Linux 单元测试与构建 |
| Windows Unit/build | success | Windows 单元测试与构建 |
| Docker integration Linux | success | Linux Docker sandbox 集成 |
| PostgreSQL integration Linux | success | 真实 PostgreSQL 17 服务上的项目管理和数据库集成 |

PostgreSQL job 使用临时 PostgreSQL 17 服务和该 job 自己的 DSN，串行执行 `go test -p 1 ./... -run 'Postgres|Management' -count=1 -timeout 10m`。凭据仅用于 CI job，本记录不包含 DSN 或密码。

## 项目与额度行为的数据库证据

以下是上述 PostgreSQL integration job 执行的源测试及其检查结果；这些表述对应测试中的断言，不外推为公网或多实例生产验收。

| 行为 | 源测试 | 检查结果 |
| --- | --- | --- |
| 项目越权与只读成员限制 | [`TestManagementHTTPPermissionsAndAtomicSubmission`](../internal/control/management_integration_test.go) | 外部用户读取项目返回 403；reader 提交实验及取消 job 返回 403 |
| 实验提交事务回滚 | [`TestPostgresManagementSubmitRollbackAndFinishOnce`](../internal/store/management_test.go) | 测试约束令第二个 job INSERT 失败；实验、job、reservation 和 reserved 计数均为 0；移除约束后相同幂等 key 可重新提交成功 |
| 同 key 并发幂等提交 | [`TestPostgresManagementConcurrentIdempotentSubmitsShareOneExperiment`](../internal/store/management_test.go) | 8 个并发相同请求得到 1 次新建、7 次复用；均指向同一实验和同 2 个 jobs；预占量为 2 |
| 每日额度并发提交 | [`TestPostgresManagementConcurrentSubmitsRespectDailyQuota`](../internal/store/management_test.go) | daily limit 为 1 时，两个同时提交只有 1 个成功，另一个返回 quota exceeded；最终 reserved 为 1 |
| 项目并发执行上限 | [`TestPostgresManagementConcurrentClaimsRespectProjectLimit`](../internal/store/management_test.go) | concurrent limit 为 1 时，两次竞争 claim 只有 1 个取得 job，另一个得到 no job；running 为 1 |
| 重复取消与重复完成 | [`TestPostgresManagementQuotaIdempotencyAndCancelAccounting`](../internal/store/management_test.go)、[`TestPostgresManagementSubmitRollbackAndFinishOnce`](../internal/store/management_test.go) | 对同一 job 重复 cancel 只释放一次 reservation；重复 finish 第二次不再更新，used 只增加一次 |
| 服务重启后的额度恢复 | [`TestPostgresManagementRecoverChargesRunningReservationOnce`](../internal/store/management_test.go) | Close 后新建 store 并 Recover：原 running job 转为 infra error 并计入 used 一次；原 queued job 保持 queued 且 reserved 保持 1；重复 Recover 不重复结算 |
| UTC 跨日结算 | [`TestPostgresManagementDailyUsageUsesReservationDay`](../internal/store/management_test.go) | 将预占日设为前一 UTC 日后完成 job，结算回原预占日，结果 reserved 为 0、used 为 1 |

`TestManagementHTTPPermissionsAndAtomicSubmission` 还覆盖 reader/outsider 的其他授权边界、同请求重放、额度拒绝和重复取消；上表仅列本次要求重点核对的断言。

## 尚未验收的范围

没有运行真实 OpenAI 或其他真实模型调用，因此没有模型能力或费用结论。浏览器视觉布局检查未完成。Docker 与 PostgreSQL 的本机集成检查没有执行；Docker 与 PostgreSQL 的集成通过仅指上述 GitHub Linux jobs。这里的结果不证明公网多租户安全性、恶意评分器强隔离或跨主机生产可靠性。
