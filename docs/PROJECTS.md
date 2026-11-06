# 项目、成员与用量管理（v1.4）

本文说明 Runtime 服务的登录、项目授权、版本化数据集、实验提交和额度结算。服务适用于可信操作者的本机部署，不是公网托管多租户产品。服务默认只监听 loopback；账号和项目角色限制应用层访问，但不能取代恶意租户隔离。

## 首次登录

1. 按 [README](../README.md) 启动 PostgreSQL 和 Runtime，并在本机打开 `http://127.0.0.1:8787/`。
2. 第一次打开页面时，通过 setup 创建首个管理员账号。只有数据库没有任何用户时 setup 才能成功。
3. 管理员在账号管理页创建其他用户。账号名由 3–64 个 ASCII 字母、数字、点、下划线或连字符组成，首字符必须是字母或数字；密码至少 12 个字符。
4. 创建项目并管理项目成员。创建人自动成为 Owner；项目资源只对其成员可见。
5. 将管理员账号加入目标项目并赋予 Owner，再由该管理员创建初始数据集版本，授予项目可用的登记任务。之后 Owner 可为已授权的任务创建新版本。

密码以 PBKDF2-SHA256（600,000 次迭代、随机盐）摘要形式保存。登录会生成 12 小时数据库会话，随机令牌摘要入库；浏览器收到 HttpOnly、SameSite=Strict cookie。登出会撤销当前会话。服务以请求 Origin 校验、CSRF 请求头和 loopback 监听保护浏览器 API。

## 管理范围与角色

管理员可以创建用户、读取全局任务目录、管理连接和密钥配置。管理员不因全局角色自动获得任何项目的数据访问权；管理员必须先成为该项目成员。请将需要操作的管理员显式加入项目并分配项目角色。

| 角色 | 查看项目 | 成员与限额 | 数据集版本 | 实验提交/取消 | Jobs、事件与用量 |
| --- | --- | --- | --- | --- | --- |
| Owner | 是 | 管理 | 创建、列出、读取 | 是 | 是 |
| Executor | 是 | 否 | 列出、读取 | 是 | 是 |
| Reader | 是 | 否 | 列出、读取 | 否 | 是 |

用户管理仅管理员可用。项目 Owner 可将已存在的账号加入项目或调整角色，并可移除成员；至少保留一位 Owner。跨项目枚举和使用其他项目的 ID 都不会授予访问权。

## 数据集版本与任务授权

数据集版本不可变，最多包含 200 个任务 ID。每个版本会保存任务输入快照，实验提交时再次校验冻结摘要。通过导入命令登记的题目先出现在全局任务目录中；目录可见不等于项目获得授权。

管理员需要先成为目标项目的 Owner，再通过创建初始版本把选中的任务分配给项目。此后项目 Owner 只能从该项目已有数据集版本包含的任务中创建新版本。任务源码、evaluator、依赖镜像与可信 grader 仍由部署管理员在本地导入和登记；项目成员不能上传任意题包、改变 Docker 参数或替换 grader。

## 创建实验

实验请求包含以下字段：

```json
{
  "name": "parser-scripted",
  "dataset_version_id": "<project dataset version id>",
  "tasks": ["go-parser-001"],
  "variants": [{"agent": "scripted", "script": "success"}],
  "repeats": 2
}
```

`tasks` 可省略，省略时运行该数据集版本的全部任务。每个 variant 必须是 `scripted` 或 `llm` 的一种有效组合；最多 20 个 variant。重复次数为 1–100，展开后的任务运行总数 `tasks × variants × repeats` 不得超过 500。LLM variant 需提供模型名；API Key 从控制器配置读取，不随请求传入。

每次 `POST /v1/manage/projects/{project_id}/experiments` 都必须包含 `Idempotency-Key`，最长 128 个可打印 ASCII 字符且不能有首尾空格。相同项目、相同 key 和相同请求返回原实验；相同项目的 key 搭配不同请求产生冲突。幂等键按项目隔离。创建实验、任务组和额度预留在一个数据库事务中提交，队列容量不足或额度不足时不会保留半组任务。

## 配额结算

每个项目有每日任务额度和同时运行任务数上限。Owner 可在项目设置中调整。每日额度使用 UTC 日期，并按任务提交日结算；一个实验里的每个 task × variant × repeat 各算一个 job。

| 额度状态 | 含义 |
| --- | --- |
| `reserved` | 已提交但尚未进入终态的 job 预留数，包含 QUEUED 和 RUNNING |
| `used` | 已运行并到达终态的 job 数；成功、失败、取消或重启中断都会计入 |
| `released` | 仍在队列时取消而释放的预留数 |
| `running` | 当前占用项目并发额度的 RUNNING job 数 |

任务提交时从 `reserved` 开始；进入 RUNNING 本身不会结算。排队期间取消会立即把 `reserved` 变为 `released`。RUNNING 任务在正常 Finish 或启动恢复终结时从 `reserved` 转为 `used`；运行中的取消请求直到任务终结和 sandbox 清理后才结算为 `used`。并发额度只计算 RUNNING job，队列长度不会受项目并发上限直接限制。运行中收到取消请求后，job 仍占一个并发位，直到执行结束和 sandbox 清理完成。

跨午夜完成的任务仍归属其提交日期。服务重启时排队 job 继续保留预留；已开始运行的 job 会在恢复时终结，并由 `reserved` 转入原提交日的 `used`。`usage` 接口可以按 UTC 日查询 `reserved`、`used`、`released`、`running` 以及当日额度和并发上限。

## 管理 API

所有路径均相对于 `http://127.0.0.1:8787`。写请求需 JSON、同源请求，并带 `X-AgentEval-CSRF: 1`；浏览器前端会自动提供该头。

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/v1/manage/auth/state` | 查询是否已完成初始化和当前会话 |
| POST | `/v1/manage/auth/setup` | 仅空数据库建立首个管理员 |
| POST | `/v1/manage/auth/login` | 账号登录并创建会话 |
| POST | `/v1/manage/auth/logout` | 撤销当前会话 |
| GET / POST | `/v1/manage/users` | 管理员列出或创建账号 |
| GET | `/v1/manage/catalog` | 管理员查看登记任务目录 |
| GET / POST | `/v1/manage/projects` | 列出可访问项目或创建项目 |
| GET / PATCH | `/v1/manage/projects/{project_id}` | 读取项目或更新限额 |
| GET / PUT | `/v1/manage/projects/{project_id}/members` | 列出或设置成员角色 |
| DELETE | `/v1/manage/projects/{project_id}/members/{user_id}` | 移除成员 |
| GET / POST | `/v1/manage/projects/{project_id}/datasets` | 列出或创建数据集版本 |
| GET | `/v1/manage/projects/{project_id}/datasets/{version_id}` | 读取数据集版本 |
| GET / POST | `/v1/manage/projects/{project_id}/experiments` | 列出或幂等创建实验 |
| GET | `/v1/manage/projects/{project_id}/experiments/{experiment_id}` | 读取实验及其任务 |
| POST | `/v1/manage/projects/{project_id}/experiments/{experiment_id}/cancel` | 取消实验中的任务 |
| GET | `/v1/manage/projects/{project_id}/jobs` | 列出项目任务 |
| GET | `/v1/manage/projects/{project_id}/jobs/{job_id}` | 读取项目任务 |
| POST | `/v1/manage/projects/{project_id}/jobs/{job_id}/cancel` | 取消任务 |
| GET | `/v1/manage/projects/{project_id}/jobs/{job_id}/events` | 分页读取执行事件 |
| GET | `/v1/manage/projects/{project_id}/usage?day=YYYY-MM-DD` | 查询指定 UTC 日用量；省略 day 查询当前日 |

旧 `/v1/runs` 路由的 GET 和写请求都已关闭，避免绕过项目授权和用量结算。旧的 `runtime submit`、`runtime batch`、`runtime experiment` HTTP 客户端不能作为 v1.4 项目服务客户端使用。`runtime run` 仍可由可信操作者本机直接运行；演示入口为 `scripts/demo.ps1` 或 `scripts/demo.sh`。这些本机操作不经过项目服务，也不计入项目配额。

## 相关指南

- [网页控制台](WEB_UI.md)
- [CLI 与配置参考](CLI_REFERENCE.md)
- [数据集与任务导入](DATASETS.md)
- [安全说明](../SECURITY.md)
