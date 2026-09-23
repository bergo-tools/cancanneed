# cancanneed（看看你的）

`cancanneed` 定期检查 Git 仓库的新提交，调用本地 coding agent 做代码审查，并通过飞书群机器人发送结果。可以同时监控多个仓库；每个仓库独立保存审查进度。

支持 Linux 和 macOS。Windows 启动时会直接报错。

## 快速开始

准备好本地 Git 仓库，以及 `pi`、`ohmypi` 或 `crush` 中至少一个 agent 的可执行文件。复制带注释的 [config.example.yaml](config.example.yaml)，修改仓库路径和 agent；不使用飞书时删除示例中的 `feishu` 和 `authors_file` 配置：

```bash
cp config.example.yaml cancanneed.yaml
# 编辑 cancanneed.yaml 中的仓库路径、agent 及可选通知配置
go build -o cancanneed ./cmd/cancanneed
./cancanneed run -config cancanneed.yaml
```

最小配置只需要仓库名称、路径和 agent 类型：

```yaml
repositories:
  - name: backend
    path: /srv/repos/backend
    agent:
      type: pi
```

`run` 是默认模式：每个仓库由一个 goroutine 独立处理，启动后立即检查一次，之后在每轮检查或审查结束后等待 `poll_interval`。只想检查一轮时运行 `./cancanneed once -config cancanneed.yaml`；它会按 `concurrency` 限制并行仓库数。`./cancanneed --help` 可查看命令用法。

## 配置

配置文件使用 YAML，未知字段会报错。相对路径以配置文件所在目录为基准；路径、`agent.env` 的值及飞书凭据中的 `${ENV_NAME}` 从 cancanneed 进程环境中展开。完整字段和每项注释见 [config.example.yaml](config.example.yaml)。

| 配置项 | 作用与默认值 |
| --- | --- |
| `repositories` | 监控的仓库列表，至少一个；每项的 `name` 必须唯一，`path` 指向本地 Git 工作区。 |
| `repositories[].remote` / `branch` | 远端默认 `origin`；未指定分支时依次尝试远端 HEAD、`main`、`master`。 |
| `poll_interval` | 每个仓库两轮检查之间的间隔，默认 `5m`；整轮审查失败后也按此间隔重试。 |
| `concurrency` | `once` 模式同时处理的仓库数，默认 `4`；`run` 模式每仓库一个 goroutine。 |
| `state_dir` / `runs_dir` | 默认为 `.cancanneed/state` 和 `.cancanneed/runs`。 |
| `max_review_runs` | 每个仓库最多保留的审查运行目录数，默认 `10`。 |
| `authors_file` | 可选的 Git 作者到飞书用户映射 JSON 文件。 |
| `feishu` | 可选的飞书群机器人配置；省略后继续审查并记录进度，但不发送消息。 |

### Agent

`repositories[].agent.type` 支持以下预设。程序会为 agent 加入自动批准参数，`command` 和 `args` 可以覆盖各自的默认命令与参数。

| 类型 | 默认命令 | 自动批准参数 | Session 行为 |
| --- | --- | --- | --- |
| `pi` | `pi` | `--approve` | 默认加入 `--no-session`。 |
| `ohmypi` | `omp` | `--auto-approve` | 默认加入 `--no-session`。 |
| `crush` | `crush` | `--yolo` | 不支持关闭 session，沿用自身行为。 |

`record_session` 默认为 `false`；设为 `true` 时，pi 和 ohmypi 不再加入 `--no-session`。`timeout` 默认为每次执行 `30m`；`retries` 默认为 `2`，即最多运行三次；`retry_backoff` 默认为 `15s`。执行超时或服务收到终止信号时，程序会终止 agent 进程组。

Agent 子进程继承 cancanneed 的环境变量，还可通过 `agent.env` 覆盖或增加变量。例如 `ANTHROPIC_API_KEY: ${ANTHROPIC_API_KEY}` 会从服务进程环境传入密钥。中文审查 prompt 直接作为命令行参数传入，不生成 prompt 文件；自定义 `args` 可使用 `{prompt}`、`{repo}`、`{branch}`、`{from_sha}`、`{submit_script}`、`{output}` 占位符。

### 飞书与作者映射

`feishu.webhook` 填飞书群自定义机器人的 Webhook 地址；启用了签名校验时再填 `feishu.secret`。这里不需要飞书应用的 App ID 或 App Secret。HTTP 超时 `feishu.timeout` 默认 `10s`。

通过 `authors_file` 可以在结果卡片里 @ 提交作者。文件格式见 [authors.example.json](authors.example.json)：

```json
{
  "Alice": {
    "feishu_id": "ou_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "name": "张三"
  }
}
```

顶层 key 对应 Git author 名称，匹配时忽略首尾空白和大小写；`feishu_id`、`name` 都必填。找不到映射时仍显示原始作者名。

## 审查如何进行

检测到新 HEAD 后，cancanneed 把上次审查的 SHA、远端和分支传给 agent。agent 自己 `git fetch`，逐个审查范围内的提交，并先读取仓库根目录下的 `review.md`（文件名忽略大小写）。首次运行、状态缺少 HEAD 或切换监控分支时，只审查当时最新的一个 commit：普通提交以第一父提交为对比起点，根提交以空树为起点。之后从已记录的 HEAD 增量审查。

审查只上报有明确证据且真正重要的问题，例如逻辑错误、崩溃、数据损坏、并发竞态、安全问题、资源泄漏和明显的接口误用。finding 的标题应直指问题，详情简洁说明触发条件、实际后果和修复方向。代码风格和纯重构偏好不在上报范围内。

提交标题或正文包含 `noreview`，或者整个提交都是第三方依赖同步、明显的自动化批量修改时，可以跳过该提交。混合提交仍需审查其中的人工修改。全部提交都可跳过时，agent 通过提交工具记录 `skip`，程序推进 HEAD，但不发送结果通知。

Agent 成功完成审查后，cancanneed 读取它审查现场的本地 `FETCH_HEAD` 作为新进度；不会在审查结束后重新读取远端。审查失败时保留原 HEAD，下次轮询继续处理。

### 提交 finding

每次审查会生成一个 `submit-review.sh`。Agent 每发现一个问题就调用一次，工具负责并发安全地写入结构化 `review.json`：

```bash
"$CANCANNEED_SUBMIT_SCRIPT" finding \
  --author "Alice" \
  --commit "0123456789abcdef0123456789abcdef01234567" \
  --file "internal/store/store.go" \
  --line 42 \
  --severity high \
  --title "写入失败后仍推进游标" \
  --detail "事务未提交时游标已更新，重试会漏掉该记录；应在提交成功后更新。"
```

`--commit` 必须是当前仓库中真实存在的完整 commit SHA。参数错误或对象不存在时，工具返回非零退出码并告知原因，agent 应修正后重试。严重级别可取 `critical`、`high`、`medium`、`low`、`info`。多个 finding 可以并发提交；agent 等所有提交命令成功后正常退出即可。没有 finding 时不需要生成 `review.json`。

全部提交都符合跳过条件时调用：

```bash
"$CANCANNEED_SUBMIT_SCRIPT" skip --reason "范围内所有 commit 均包含 noreview"
```

如果 agent 无法 `git fetch`（例如网络或远端权限错误），应立即报告并结束本轮审查：

```bash
"$CANCANNEED_SUBMIT_SCRIPT" fetch-failed --reason "git fetch origin main: permission denied"
```

该指令会废弃本轮已提交的部分 finding；cancanneed 将本轮视为失败，保留原 HEAD，并在下次轮询重试。提交工具成功写入失败标记后，agent 直接退出即可。检测远端更新时遇到 Git 错误也会按审查失败记录。

## 通知与失败重试

发现重要问题后，cancanneed 从本地 Git 读取相关 commit 的标题、作者、邮箱和提交时间。结果卡片标题为“Code Review结果通知”，按作者分卡，并在每位作者下面按 commit 展示 finding；同一 commit 的 finding 不会被拆开。单卡以 8 个 finding 为拆分目标，内容过多时会发送多张卡片。没有 finding 或全部跳过时只推进 HEAD，不发送结果通知。

飞书发送进度保存在仓库 state 中。发送失败不会重新审查已完成的提交，也不会阻止后续审查；下次轮询会从未发送成功的卡片继续。若进程恰好在发送成功、进度落盘之前退出，重启后可能重发该卡片。

Agent 执行失败会先按 `retries` 和 `retry_backoff` 在当前轮重试。整轮仍失败时，首次发送“Code Review失败通知”；同一待审查 HEAD 的后续失败只在累计第 30、60、90……次时再次通知。审查前的仓库检查（包括读取远端 HEAD）有 5 分钟总超时；这类失败独立计数，首次和每累计 30 次通知一次，尚无法提供 HEAD 时在卡片中标为“未获取”。仓库检查恢复后清空其失败计数，审查成功后清空 agent 审查失败计数；服务正常退出不计为失败。

## 状态与运行文件

默认目录位于配置文件所在目录下：

```text
.cancanneed/
├── state/
│   ├── backend.json              # HEAD、失败计数、待发通知及发送进度
│   └── backend.json.lock         # 此仓库的进程锁
└── runs/
    └── backend-<sha>-<时间戳>/
        ├── request.json          # 本次审查请求
        ├── submit-review.sh      # finding/skip/fetch-failed 提交工具
        ├── agent-attempt-1.stdout.log
        ├── agent-attempt-1.stderr.log
        └── review.json           # 有 finding、skip 或 fetch-failed 时生成
```

每个仓库各用一个状态文件和锁。同一仓库不能同时由两个实例监控；仓库集合不重叠的实例可以共用 `state_dir`。进程结束后系统会释放锁，`.lock` 文件仍可保留。要让某个仓库重新按“首次审查”处理，请先停止监控它的实例，再删除对应的 JSON 状态文件。

`max_review_runs` 会在启动检查及新审查前清理该仓库最旧的运行目录，不影响其他仓库。运行日志和结果可能包含代码及 agent 输出，应限制这些目录的访问权限。生产环境建议使用权限受限的服务账号和专用仓库副本，不要把真实密钥写入配置文件。

## 开发与测试

项目提供不调用模型的 dummy agent，可用于端到端验证重试和结构化提交：

```bash
go build -o /tmp/cancanneed-dummy ./cmd/dummy-agent
go test ./...
go vet ./...
```

测试时可把仓库的 `agent.command` 设为 `/tmp/cancanneed-dummy`，并通过 `agent.env` 设置 `DUMMY_FAIL_COUNT` 和 `DUMMY_COUNTER_FILE`。具体配置字段仍以 [config.example.yaml](config.example.yaml) 为准。
