# cancanneed（看看你的）

`cancanneed` 是一个常驻的 Go 服务：轮询多个 Git 仓库，在受监控分支出现新提交时启动本地 coding agent 做增量 review，并把结构化结果发送成飞书卡片。

支持的 agent 预设：

- `pi`：自动加入 `--approve`；`record_session: false` 时加入 `--no-session`
- `ohmypi`：自动加入 `--auto-approve`；`record_session: false` 时加入 `--no-session`
- `crush`：自动以 `crush --yolo run ...` 运行；当前不支持关闭 session

每个预设都可以覆盖 `command` 和 `args`，因此 CLI 版本差异不需要改代码；cancanneed 仍会规范化并加入对应 agent 的自动批准参数。`args` 支持 `{prompt}`、`{repo}`、`{branch}`、`{from_sha}`、`{submit_script}` 和 `{output}` 占位符。Prompt 直接作为进程参数传给 agent，不落地成文件。

`agent.record_session` 默认为 `false`，仅对原生支持 `--no-session` 的 pi 和 ohmypi 生效；设为 `true` 时保留它们的 session。Crush 没有该参数，因此保持 Crush 自身的 session 行为。`agent.timeout` 控制单次 agent 进程的最长执行时间，默认 `30m`，每次 `agent.retries` 重试分别计算超时；Linux/macOS 下超时或服务取消会终止整个 agent 进程组，避免其测试和 shell 子进程残留。

## 工作流

1. 常驻模式为每个仓库启动一个独立 goroutine。每个 goroutine 启动后立即检查一次；本轮检查或 review 完成后等待 `poll_interval`，再检查下一次。单个仓库的慢 review 不会阻塞其他仓库，同一仓库也不会出现重叠 review。
2. 第一次看到仓库、state 中没有历史 HEAD，或监控分支发生切换时，只 review agent 最终 fetch 到的最新一个 commit：普通提交以第一父提交为对比起点，根提交以 Git 空树为起点。review 成功后才把该现场的 `FETCH_HEAD` 写入 state，不会回溯审查整个历史。
3. HEAD 改变后生成一次运行目录，里面只包含运行元数据、日志、`submit-review.sh` 和最终的 `review.json`；不会生成 `prepare-review.sh` 或 `prompt.md`。`max_review_runs` 控制每个仓库最多保留多少个运行目录，默认 `10`，启动检查和新 review 前都会删除该仓库最旧的超额目录，不影响其他仓库。
4. cancanneed 把上次 HEAD 注入 prompt 和 `CANCANNEED_FROM_SHA`，并通过 `CANCANNEED_REMOTE`、`CANCANNEED_BRANCH` 提供目标远端与分支。agent 自己 fetch 远端并完成增量对比，退出前再次 fetch；如果 `FETCH_HEAD` 变化就继续检查。agent 正常退出后，cancanneed 直接读取本地 `FETCH_HEAD` 作为实际审查终点，不会再查询远端并误记审查结束后才出现的提交。
5. agent 逐个覆盖范围内的 commit，并首先读取仓库根目录下忽略大小写匹配的 `review.md`。只上报逻辑错误、崩溃、数据损坏、并发、安全、资源泄漏、明显接口误用等真正重要的问题。
6. 标题或正文带 `noreview` 的 commit，以及完全由 vendor/第三方同步或明显自动化批量修改组成的 commit 可以跳过；混合 commit 仍需检查其中的人工修改。
7. 每发现一个问题，agent 调用一次 `submit-review.sh finding ...`，传入 author、commit、文件、行号、严重级别、标题和说明。不同 finding 可以并发提交；工具通过跨进程锁和原子替换避免丢失更新。
8. 所有 finding 提交完成后，agent 直接正常退出，不需要调用 `complete`。退出码为 0 就表示本次 review 完成；没有 finding 时不需要生成 `review.json`。
9. 全部 commit 都可跳过时调用一次 `submit-review.sh skip --reason "..."` 后正常退出：推进 HEAD，但不发送飞书通知。
10. agent 内部的单次执行失败按 `agent.retries` 和 `agent.retry_backoff` 回退重试；整轮 review 仍失败时保留原 HEAD，并在 `poll_interval` 后重新审查同一范围。服务正常取消或退出不会被计为 review 失败。
11. 每个待审查 HEAD 首次失败会发送“Code Review失败通知”。同一 HEAD 持续失败时不逐次通知，只在累计第 30、60、90……次失败时再次报告累计失败及重试次数；review 成功后清空该 HEAD 的失败计数。
12. 正常 review 成功后原子更新 state，并把结果加入持久化通知队列。飞书发送失败不会触发重复 review，也不会阻止后续 HEAD 继续审查；恢复后按队列顺序补发。结果卡片标题固定为“Code Review结果通知”，按 author 分组，每张卡片只包含一个作者，并在作者下面按 commit 展示 finding。同一作者超过 8 个 finding 时会拆成多张卡片；发送进度写入 state，中途失败后从第一张未成功的卡片继续。

## 快速开始

```bash
cp config.example.yaml cancanneed.yaml
go build -o cancanneed ./cmd/cancanneed
./cancanneed run -config cancanneed.yaml
```

只执行一轮，适合 cron、调试或首次审查最新 commit：

```bash
./cancanneed once -config cancanneed.yaml
```

`once` 模式会按 `concurrency` 限制并行仓库数；常驻 `run` 模式固定为每个仓库一个 goroutine。

`state_dir` 默认为配置文件目录下的 `.cancanneed/state`。每个仓库独立保存状态，例如：

```text
.cancanneed/state/backend.json
.cancanneed/state/frontend.json
```

每个文件只包含对应仓库的 HEAD、失败计数和通知队列。停止 cancanneed 后，可以单独删除某个 JSON，让该仓库下次按“无历史 HEAD”处理，而不影响其他仓库。普通字母、数字、点、下划线和连字符组成的仓库名会直接作为文件名；其他名称会转换成安全名称并附加稳定哈希，避免冲突。

每个仓库状态有独立进程锁。同一个仓库不能被两个 `run`/`once` 实例同时监控，但只要仓库集合不重叠，多个实例可以共用同一 `state_dir`。

配置使用严格 YAML，未知字段会被拒绝。相对路径以配置文件所在目录为基准，字符串中的 `${ENV_NAME}` 会从 cancanneed 进程的环境变量展开。完整配置见 [config.example.yaml](config.example.yaml)，其中只保留一个示例仓库并为每个配置项提供了注释。

可通过顶层 `authors_file` 指向一个外部 JSON 文件，把 Git author 映射为飞书用户。示例见 [authors.example.json](authors.example.json)：

```json
{
  "Alice": {
    "feishu_id": "ou_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "name": "张三"
  }
}
```

JSON 顶层 key 必须与 finding 中的 Git author 名称对应，匹配时忽略首尾空白和大小写；`feishu_id` 和 `name` 都是必填项。匹配成功后，作者卡片会显示并 @ 对应飞书用户；没有匹配时继续显示原始 Git author，不影响卡片发送。

`feishu.webhook` 填飞书群自定义机器人的 Webhook 地址（`https://open.feishu.cn/open-apis/bot/v2/hook/...`），cancanneed 会直接向该机器人发送交互式卡片；不需要飞书应用的 App ID 或 App Secret。机器人如果启用了“签名校验”，再配置对应的 `feishu.secret`。不配置 `feishu` 时仍会完成 review 和状态推进，只是不发送消息。

## 结构化提交工具

Agent 不直接拼装 JSON。每个问题调用一次：

```bash
"$CANCANNEED_SUBMIT_SCRIPT" finding \
  --author "Alice" \
  --commit "0123456789abcdef0123456789abcdef01234567" \
  --file "internal/store/store.go" \
  --line 42 \
  --severity high \
  --title "写入失败后仍推进游标" \
  --detail "更新游标前需要确认事务已经提交。"
```

如果所有 commit 都属于允许跳过的情况，调用：

```bash
"$CANCANNEED_SUBMIT_SCRIPT" skip \
  --reason "范围内所有 commit 均包含 noreview"
```

普通 review 不需要执行任何结束命令。等待所有 finding 命令返回后，让 agent 以退出码 0 正常退出即可。如果有 finding，工具生成如下 JSON：

```json
{
  "findings": [
    {
      "severity": "high",
      "author": "Alice",
      "commit": "0123456789abcdef0123456789abcdef01234567",
      "file": "internal/store/store.go",
      "line": 42,
      "title": "写入失败后仍推进游标",
      "detail": "更新游标前需要确认事务已经提交。"
    }
  ]
}
```

`skip` 只能用于所有 commit 都无需审查的情况，不能与 finding 共存，且不会发送飞书通知。其他情况下，cancanneed 根据 finding 数量自动生成 verdict 和摘要：没有 finding 为 `approve`，有 finding 为 `request_changes`。严重级别可取 `critical`、`high`、`medium`、`low`、`info`。每次运行的完整 stdout/stderr 和产物保留在 `runs_dir`。

## Dummy agent 与测试

项目包含一个不调用模型的 dummy binary，可用于端到端验证重试和结构化提交：

```bash
go build -o /tmp/cancanneed-dummy ./cmd/dummy-agent
```

在某个仓库的 agent 配置中使用：

```yaml
agent:
  type: pi
  command: /tmp/cancanneed-dummy
  args: ["{prompt}"]
  retries: 2
  retry_backoff: 100ms
  env:
    DUMMY_FAIL_COUNT: "1"
    DUMMY_COUNTER_FILE: /tmp/cancanneed-dummy-attempts
```

然后运行：

```bash
go test ./...
go vet ./...
```

测试覆盖配置默认值、agent 失败重试、并发 finding 提交、严格 JSON、首次最新 commit 与后续更新的完整流程、持久化状态和飞书卡片。

## 运行安全

coding agent 本身能执行命令，prompt 约束不是安全边界。生产环境建议给每个仓库使用专门的只读 clone，并把 cancanneed/agent 放在权限受限的容器或系统账号中；API key 通过进程环境注入，不要提交到配置文件。运行产物可能包含代码和 agent 输出，应限制 `runs_dir` 的访问权限并按需清理。
