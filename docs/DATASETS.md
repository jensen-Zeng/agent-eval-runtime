# 数据集接入、重复实验与证据导出

本文说明如何把可审核的 Go 代码修复任务接入 AgentEval Runtime，完成环境准备、重复实验和证据导出。任务源码进入 Agent 可见 workspace；公开指令作为 prompt；grader 由评测器读取，按可信输入管理。当前任务源码与内置测试支持范围是 Go。操作者必须信任自己导入的 grader、依赖和基础镜像；这套边界不提供针对恶意 grader 或公网多租户的强隔离。

## 接入前整理题目包

每道任务应有清楚分离的源码、公开指令和可信 grader。题面只写 Agent 完成任务需要知道的信息；不要把答案、参考补丁、历史 submission、内部评分说明或隐藏测试复制进 Agent 可见的 source/prompt。grader 和隐藏测试只放在单独的 `grader_dir`，按受信任代码审核。检查路径、许可和来源后再提交清单；清单中的状态 `candidate` 代表等待人工确认，不代表题目已通过基线。

通用清单模板位于 `examples/dataset-manifest.json`。先复制并按自己的数据布局修改清单：`root` 必须是数据集根目录的绝对路径；`source`、`prompt_files`、`grader_dir` 等字段均相对于该根目录；`source` 可指目录、ZIP、TAR、TAR.GZ/TGZ，归档内有多个源码目录时可设置 `source_member` 选择子目录。source 应是 Go module，依赖镜像构建要求源码根有 `go.mod`，并可选 `go.sum`。`allowed_files` 用于把允许修改的文件范围显式写进任务；`metadata` 可记录审阅过的上游 revision、数据集 split 或来源说明。

模板中的路径和 task ID 是占位示例。请在运行前把 `root`、源码/题面/grader 路径和任务 ID 改成与本机文件相符的值，并确保 prompt 文件为 UTF-8。文件清单仅包含相对数据集根目录的路径，避免把本机绝对路径写进任务材料或版本库。

## 可选：用 E3 目录适配器发现候选

当现有数据按 E3 题包目录结构组织时，可用 `e3` adapter 扫描；它只负责发现候选，不会替操作者确认题面公开性、grader 可信度或题目质量。`dataset scan` 当前支持 `e3` adapter；其他数据集使用手写通用 manifest。

```powershell
New-Item -ItemType Directory -Force artifacts/datasets | Out-Null
$datasetRoot = 'YOUR_DATASET_ROOT' # 替换为本机题包根目录的绝对路径
.\bin\runtime.exe dataset scan --adapter e3 --root $datasetRoot --output artifacts/datasets/catalog.json
```

扫描结果包含稳定 catalogue key、task ID、来源哈希、输入边界、状态和备注。若出现重复 ID，应使用完整 key 选择具体副本；CLI 不会猜测哪个版本最新。常见状态包括 `candidate`、`missing_grader`、`ambiguous` 和 `incompatible`。只有经人工审核且状态为 `candidate` 或 `ready` 的项可导入；不要仅把状态改成可导入值来绕过审核。扫描和报告输出都要求目标文件是新文件，保留既有报告并改用新的输出名。

## 预检并导入任务

先在仓库根目录创建报告目录并构建 CLI：

```powershell
New-Item -ItemType Directory -Force artifacts/datasets, artifacts/experiments | Out-Null
.\scripts\build.ps1
```

手写 manifest 不需要先运行 E3 扫描。下例假设你已把 `examples/dataset-manifest.json` 中的占位路径替换为实际数据路径，并准备导入 `sample-v1`：

```powershell
.\bin\runtime.exe dataset import --project . --file examples/dataset-manifest.json --select sample-v1 --dry-run --output artifacts/datasets/precheck.json
.\bin\runtime.exe dataset import --project . --file examples/dataset-manifest.json --select sample-v1 --output artifacts/datasets/import.json
```

`--select` 接受 catalogue key 或唯一 task ID；有重复 ID 时必须给精确 key。`--prefix` 可选，用于目标 registry task ID 命名空间；上游 task ID 保存在 metadata 中。`--dry-run` 会预检选中任务、路径、源码、指令、grader 和 manifest，不会注册任务。正式导入会逐项写入结果；若有题目被拒绝，报告会保留原因并继续处理其余选中项，命令以退出码 2 表示部分失败。实际导入报告要与 dry-run 报告分开保存；只有实际导入报告可用于生成实验矩阵。新任务注册后重启服务以加载 registry。若任务使用离线依赖镜像，应先按下一节生成依赖镜像，再运行正式导入时追加 `--module-cache /opt/agenteval/go-mod`，使任务 manifest 与镜像缓存路径一致；dry-run 可以先执行，不使用依赖缓存时省略该 flag。

若需要调整预算，先创建下述文件，再在导入命令追加 `--profile artifacts/datasets/dataset-profile.json`。可创建本地 `artifacts/datasets/dataset-profile.json`，内容使用真实 profile schema：

```json
{
  "budget": {
    "run_seconds": 900,
    "eval_seconds": 300,
    "max_steps": 50,
    "max_tokens": 30000
  },
  "limits": {
    "cpus": 1,
    "memory_mib": 1024,
    "pids": 64,
    "workspace_mib": 128
  }
}
```

profile 中省略的字段保留导入默认值。当前导入默认预算包括总运行 900 秒、评测 300 秒、最多 50 步；默认资源为 1 CPU、1024 MiB 内存、64 个 PID、128 MiB workspace，`tmp_mib` 省略时采用默认 tmpfs。策略上限包括 run 1800 秒、eval 300 秒、CPU 4、内存 2048 MiB、PID 256、workspace 128 MiB；`tmp_mib` 可省略，或设置 64–1024 MiB 且不能超过内存限额。所有值仍要满足任务校验规则。profile 是受信任操作者的配置，不应接收未经审核的外部 JSON。

重复导入时可传 `--if-exists verify`。它只会复用源码、grader、指令、metadata、预算及环境配置完全一致的既有任务；发现漂移会拒绝，不会覆盖。需要修改输入时应使用新的 task ID 或经审核的版本。

## 为 Go 依赖生成离线镜像

运行阶段不会下载 Go 依赖，也不会自动安装工具链。对需要模块依赖的题目，`dataset image` 可从已审核清单生成一个依赖专用的 Docker build context：

```powershell
.\bin\runtime.exe dataset image --file examples/dataset-manifest.json --select sample-v1 --base-image agenteval-sandbox:v1.0 --output artifacts/datasets/sample-deps-context
docker build -t agenteval-sample-deps:v1 artifacts/datasets/sample-deps-context
```

`--base-image` 必须是本机可用的镜像 tag，或 `repository@sha256:...` digest；不能传裸 image ID。生成的 context 仅含选中任务的 `go.mod`/`go.sum` 和构建说明，不带源码、prompt、grader 或答案。构建阶段下载并缓存 Go modules；运行阶段将 `GOPROXY=off`、`GOSUMDB=off`、`GOTOOLCHAIN=local`，缓存放在 `/opt/agenteval/go-mod`。这种生成方式要求每个选中任务的源码根目录有 Go module；有本地 `replace`、多个 module 或系统级依赖的任务需要单独准备并审核镜像。先以可信镜像完成真实 baseline，避免把依赖/镜像问题误记为模型失败。报告会冻结任务实际使用的镜像 ID，tag 后续移动不会改变已有运行记录。

将依赖镜像配置到控制器时，保留 `configs/local.json` 作为本机配置基线。复制 `configs/local.json` 为 `configs/dependency.example.json`，只在副本中把顶层 `image` 改为新镜像 tag，再启动：

```powershell
Copy-Item configs/local.json configs/dependency.example.json
# 编辑副本的顶层 image 为 agenteval-sample-deps:v1
.\scripts\start.ps1 -ConfigFile configs/dependency.example.json -SkipPrepare
```

`-SkipPrepare` 表示跳过默认基础镜像准备；脚本仍会构建 CLI、等待 PostgreSQL、检查配置并启动服务。PostgreSQL 密码由脚本从环境变量或交互输入读取，模型 API key 使用 `OPENAI_API_KEY` 等配置的环境变量名；不要把真实密钥写入清单、profile、题包或公开配置。复制出来的配置副本应只含非敏感默认值和环境变量名称；不要把真实 API key、数据库密码或本机专属值提交到公开仓库。本指南只说明复制和修改方式，不创建或覆盖 `configs/dependency.example.json`。

## 创建矩阵并运行实验

导入成功后，用实际导入报告创建 baseline 矩阵或模型矩阵。空 `--models` 会生成 baseline-only 矩阵；带模型名时默认只创建模型 variant，`--baseline` 可同时加入 baseline：

```powershell
# 无模型费用的 baseline，每题重复两次
.\bin\runtime.exe dataset matrix --report artifacts/datasets/import.json --name sample-baseline --repeats 2 --output artifacts/datasets/baseline-matrix.json

# 多模型比较，并加入 baseline
.\bin\runtime.exe dataset matrix --report artifacts/datasets/import.json --name sample-models --models MODEL_A,MODEL_B --repeats 3 --baseline --output artifacts/datasets/model-matrix.json
```

矩阵按 task × variant × repeats 展开为独立 jobs；当前单个矩阵最多 1000 jobs，repeats 范围为 1–100。矩阵只会收录实际成功导入或校验一致的任务，拒绝项留在原导入报告中。随后提交并等待完成：

```powershell
.\bin\runtime.exe experiment --file artifacts/datasets/model-matrix.json --output artifacts/experiments/sample-models.json --csv artifacts/experiments/sample-models.csv --wait --timeout 30m
```

中断后用相同 matrix、manifest 和 CSV 路径恢复原实验，只额外添加 `--resume`：

```powershell
.\bin\runtime.exe experiment --file artifacts/datasets/model-matrix.json --output artifacts/experiments/sample-models.json --csv artifacts/experiments/sample-models.csv --wait --timeout 30m --resume
```

`--resume` 要求已有 experiment manifest 且输入矩阵完全相同；它沿用原 job ID 并继续收集未完成工作，不会重新执行已完成的 job。新一轮实验应使用新的矩阵名称和输出路径。服务需要配置正确的模型与 API key 环境变量；API key 本身不进入 matrix 或任务材料。

PowerShell 一键脚本可从导入报告创建矩阵、运行实验并导出 bundles：

```powershell
.\scripts\run-dataset.ps1 -ImportReport artifacts/datasets/import.json -Models @('MODEL_A','MODEL_B') -Repeats 3 -IncludeBaseline
```

该脚本会生成唯一输出目录，默认产物位于 `artifacts/dataset-runs/`。如需恢复中断实验，使用脚本打印的原 experiment 路径，直接调用 `runtime experiment --resume`；不要重新调用一键脚本来假设它会复用旧任务。

## 导出可审查证据

单次运行可导出 patch、trajectory 和机器报告：

```powershell
.\bin\runtime.exe export --run artifacts/RUN_ID --output artifacts/datasets/submission-RUN_ID
```

整个实验可按 run 导出 bundle 并生成索引：

```powershell
.\bin\runtime.exe dataset export --experiment artifacts/experiments/sample-models.json --artifacts artifacts --output artifacts/datasets/sample-models-bundles
```

单次 bundle 包含 `candidate.diff`、`trajectory.jsonl`、`run.json`、可用时的 `evaluation.json`、`AI_USAGE.json`、`README.md` 和 `BUNDLE_MANIFEST.json`。实验 bundle 保留 task/model/repeat/job/run 对应关系；未执行或材料不完整的项会标明跳过，不会伪造轨迹。导出要求输出目录为新目录，并校验路径、文件类型、报告和补丁哈希。

导出的内容是机器执行证据；成功率、partial score、基础设施错误和未知 usage 需要结合报告解释。它不自动生成任何人工推理、自测论证或签署记录。发布 evidence 前应再次检查 prompt、trace、diff、日志及 metadata 是否包含密钥、私有题面或其他不应公开的内容。

## 当前支持范围

当前运行与内置 evaluator 面向 Go 代码修复任务。manifest 可描述数据集文件边界，但声明其他编程语言不代表其执行器和评测策略已具备该语言支持。评测使用的 grader 是可信输入；只导入明确的 grader 目录，不要把整份作业目录、oracle、隐藏答案或历史作答传入。E3 adapter 仅用于特定目录结构的候选发现，通用 manifest 是新数据集的主要接入方式。