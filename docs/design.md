# git-tab 设计：目的、主线、取舍

## 目的

每个 commit 自动带一张 AI 小票：这次提交用 coding agent 花了多少 token、按 API 价折合多少美元、用了哪些模型。小票写进 commit message 的 trailer，跟着仓库走，换电脑不丢，不需要服务端。几年后翻 `git log` 能看到当时的价钱，也能按项目、模型、任务类型复盘。

硬约束三条：绝不挡住 commit；写进历史的格式改不了，所以带版本号；只读本地 session 文件，不依赖中转。

## 照着谁抄

| 部分 | 范本 | 抄了什么 |
|---|---|---|
| session 解析 | [yetone/magpie](https://github.com/yetone/magpie) `internal/sessions/claude*.go`、`codex*.go`（Go，MIT，21.8k star） | Claude Code 按 `message.id + requestId` 去重；Codex 用每次调用的 `last_token_usage`，累计值相同的重复事件跳过，fork 出来的子线程重放父线程检查点时 `last` 为 0，因此不会重复记账；`token_usage_record` 优先；`.jsonl.zst` 解压 |
| 计价 | magpie `internal/catalog/catalog.go` + [models.dev](https://models.dev/api.json) | 价格单位 USD/百万 token；按单次请求的 prompt 大小选 context 分档；Claude 1 小时缓存写入按 2 倍输入价（models.dev 没有这一项） |
| trailer 落位和 commit source 策略 | [trusty-mpm `commit_trailers.rs`](https://github.com/bobmatnyc/trusty-tools/blob/main/crates/trusty-mpm/src/core/commit_trailers.rs) | trailer 必须在最后一段且首行是 trailer；插在末尾注释块和 `--verbose` 剪刀线之上；merge/squash 不打；amend/cherry-pick 先剥掉继承来的旧块再打 |
| 一行格式 | [Scopeon](https://github.com/sorunokoe/Scopeon) 的 `AI-Cost:` trailer | key 名沿用 `AI-Cost` |

## 主线

```
git commit
  ├─ prepare-commit-msg ─┐ 消息已有内容（-m / -F / --amend）：当场算账、写入
  ├─ （编辑器）            │
  ├─ commit-msg ─────────┘ 消息原本是空的（编辑器流程）：编辑器关闭后再算账、写入
  └─ post-commit           确认 commit 成功，才把这次的用量记为"已结算"
```

算账：读 `~/.claude/projects/**/*.jsonl` 和 `~/.codex/{sessions,archived_sessions}/**/rollout-*.jsonl[.zst]`，取 cwd 落在本仓库工作树内、且尚未结算的用量事件，逐请求计价后汇总。

结算账本放在 `<git-common-dir>/git-tab/ledger.json`，只在本机：

- `base`：在它之前的用量都已结算。第一次运行时取 HEAD 的提交时间。
- `marks[thread]`：某个 session 线程（主线程或子代理）单独结算到的时间点，优先于 `base`。agent 自己 commit 时只推进它自己线程的 mark，父线程或兄弟线程的用量留给之后的 commit。
- 仓库有多个 worktree 时（比如子代理在 `.claude/worktrees/` 里并行施工），账本是共用的，而每个 worktree 只看得到自己目录下的用量。这时人手 commit 也只推进本次计费线程的 mark，不动 `base`，否则会把其他 worktree 还没提交的用量一并结算掉（`TestWorktree` 覆盖）。
- 比 `tab.lookback` 还旧的 mark 会被删掉：那么旧的用量本来就不计。只有最后一个 commit 保留结算前的完整状态，供 amend 回滚。

归因两档：

1. **精确**：commit 由 agent 发起时，环境里有 `CLAUDE_CODE_SESSION_ID`（Claude Code 2.1.308 会传给 Bash 子进程，本机实测）或 `CODEX_THREAD_ID` / `CODEX_SESSION_ID`（[codex-rs `shell_environment.rs`](https://github.com/openai/codex/blob/main/codex-rs/protocol/src/shell_environment.rs)）。只结算这个 session（含其子代理），只推进它自己的 mark。其他并发 session 的用量留给下一次 commit。
2. **按仓库**：人手动 commit，没有 session 环境变量。结算所有 cwd 在本仓库的 session，`base` 推到现在。

跨机器：账本只在本机，小票随 commit 走。每张小票用 `AI-Session: agent/会话 id 末 8 位` 写明结算了哪些会话。stamp 时读一遍 lookback 窗口内的 `git log`，凡是不在本机账本里的 commit（别的机器、云端容器打的戳）所点名的会话，把它在那个 commit 作者时间之前的用量视为已结算。这样 `claude --teleport` 把云端对话拉回本地、或者 instabox 之类工具把对话搬到另一台机器时，同一段用量不会记两次（`TestTwoMachines` 覆盖）。用作者时间是因为 rebase 会改提交时间、不改作者时间。

实付：`tab-plan.<名>.*`（全局 git config，或云端用的 `GIT_TAB_PLANS` 环境变量）登记「什么由谁付、按什么比例」。「谁收钱」取日志里写明的服务商，没写才取模型厂商；按 agent/谁收钱 → 谁收钱 → agent 的顺序匹配。谁收钱排在单独的 agent 前面，是因为经 magpie 之类网关把 Claude Code 转给 DeepSeek 时，这部分不该算进 Claude Max；日志写明服务商时不再看厂商，是因为 OpenCode Go 卖的 DeepSeek 模型不该算到 DeepSeek 官方 key 上。订阅的比例 = 月费 ÷ 一个月用量的 API 价：用户自报 `--worth` 优先，没有就从本机 30 天日志测，日志不足 30 天按比例放大，每周在后台重测一次。本机日志看不到 claude.ai 网页端和其他设备上的用量，所以自报通常更准。

累计账单：receipt 模板最后一行是当前分支历史里所有小票之和，从 `git log` 的 trailer 里加，所以别的机器、队友的 commit 也算在内。结果按 HEAD 缓存在账本里，下一个 commit 直接在缓存上加一笔，不用重新遍历历史（本机最大的仓库 2472 个 commit，遍历一次约 60ms）。

amend：账本记着每个 commit 结算前的状态。amend 时回滚到被改那个 commit 之前的状态重新算，所以 amend 后的小票是累计值，旧块会被剥掉换成新块。`--amend -m` 时 git 给的 source 是 `message`，靠读父进程命令行里的 `--amend` 识别。

## 取舍

| 决定 | 用什么换什么 |
|---|---|
| Go 单二进制 | hook 每次 commit 都跑，启动要快（二进制自身启动约 5ms），交叉编译和 `go install` 都简单；换来的代价是社区写 adapter 要会 Go |
| 只依赖 `klauspost/compress`（v1.20.3） | Go 1.27 标准库还没有 zstd；纯 Go 实现免 cgo，交叉编译不受影响 |
| 全局安装用 git 配置式 hook（`hook.<name>.command`，Git 2.54 起，见 [RelNotes 2.54.0](https://github.com/git/git/blob/master/Documentation/RelNotes/2.54.0.adoc) 第 67 行） | 和 husky、pre-commit 等仓库自带 hook 共存，不碰 `core.hooksPath`；旧版 git 不支持全局，只能逐仓库装 hook 文件（原有 hook 改名后串联执行） |
| 写 trailer，不用 git notes | notes 默认不随 push/clone 走，违背"跟着仓库走"；代价是 trailer 写进去就改不了 |
| 成本标 `(api-equiv)` | 订阅用户不按 token 付费，数字是"按公开 API 价折算"，trailer 里明说 |
| 价格在 commit 当时定死 | 小票的意义就是当时的价；以后调价不回改历史 |
| 价格表：内嵌 models.dev 快照，`git tab prices update` 刷新到本机缓存；缓存超过 7 天时 post-commit 在后台静默刷新（`tab.autoUpdatePrices=false` 关闭） | hook 本身从不联网，联网只在后台子进程里，失败无感 |
| 默认模板 receipt（小票 ASCII 框 + 标准 trailer） | 一眼能看出是小票，代价是 commit message 多 20 多行；嫌长可以 `tab.template standard` 只留 trailer。minimal 一行不带版本号行，靠 `(api-equiv)` 标记识别为 schema 1；以后改格式必须写 `AI-Cost-Schema: 2` |
| 订阅折扣用「月费 ÷ 月用量 API 价」一个比例 | 简单、可解释、和用户心算一致；代价是忽略了订阅的 5 小时窗口和周上限，用量越多比例越低 |
| `AI-Session` 只写会话 id 末 8 位 | 公开仓库里不暴露完整会话 id；8 位十六进制随机部分足够在一个仓库内区分会话 |
| 云端靠仓库 `.claude/settings.json` 的 SessionStart 钩子安装 | Claude Code 云端会话不加载插件、只跑仓库里提交的钩子（[官方文档](https://code.claude.com/docs/en/cloud-environments)）；代价是要把这个钩子提交进仓库 |
| 不做 magpie 插件 | magpie 的插件（provider/middleware/routing/tools/page/agent）都碰不到 git；能做的是读它的数据，宣传走它的 Discord |
| merge、squash、rebase 不打戳；cherry-pick/revert 只剥旧块 | 这些 commit 的消息由 git 拼出来，用量不属于它们；没结算的用量留给下一个普通 commit |
| fast 模式按 2 倍计价 | Opus 5 / 5.5 官方价就是 2 倍；Opus 4.8 的 fast 价没查到，同样按 2 倍算（推断） |
| 用量查找窗口最多回看 30 天（`tab.lookback`） | 老仓库第一次 commit 不会扫出几个月的用量 |
| hook 内部 4 秒超时（`tab.timeout`） | 超时就不写小票，commit 照常 |
| 每个 hook 最多起 2 个 git 子进程（一次 `rev-parse` 拿全路径和 HEAD，一次 `config`）；commit-msg、post-commit 先直接读 pending 文件，没活就退出，不起 git | 每个 git 子进程约 7ms。改之前一次 commit 多花约 170ms，改后多花约 45ms（20 次中位数 61ms 对 16ms，本机 M5）；代价是靠「消息文件在 git 目录里、hook 在工作树根目录运行」这两条 git 惯例定位 git 目录，猜不到时退回完整流程 |
| `install.sh`：在 checkout 里运行就地编译，用 curl 管道运行就下 Release 二进制（`.goreleaser.yaml` + 打 tag 触发的 workflow），没有 Release 再退回 `go install` | 一条命令装、一条命令卸；Release 要等仓库公开、打 tag 才有 |

## 已知不准的地方

- Codex 的 priority 档（2 倍价）还没读，按标准价算。
- 一个 Claude Code session 从上级目录启动、同时改两个仓库时，精确模式下上级目录 cwd 的用量可能被两个仓库各算一次。
- GitHub 网页上的 squash merge 不走本地 hook，被 squash 掉的分支 commit 的小票不会进主干历史。
- 子代理按父 session 归因；Claude Code 的 `<synthetic>` 消息和 API 报错消息不计费。
- 同一个线程先后在两个 worktree 里干活时，mark 按线程记，先提交的那个 worktree 会把这条线程在另一个 worktree 里更早的用量一并结算掉。
- DeepSeek 高峰时段按北京时间工作日 9–12、14–18 点双倍算，不认中国法定节假日和调休。
- Claude Code 经网关转发给别家模型时，按它日志里写的模型名计价。
- 只有最近一个 commit 能 amend 出累计小票；`reset` 回去再 amend 更早的 commit，小票只算上次结算之后的用量。

## 工作包和依赖

```
agents (session 解析，adapter 注册表) ──┐
price  (models.dev 价格表，逐请求计价) ─┼─> bill (按窗口过滤、汇总) ─> render (四种模板、消息落位)
gitx   (git 调用)  ──┬─> ledger (结算账本) ┘                              │
                     └──────────────────────> stamp (三个 hook 的编排) <──┘
install (装卸 hook) / report (summary、receipt、badge、累计账单) / plan (订阅与实付) / cli (子命令分发)
```

| 包 | 接口 | 验收 |
|---|---|---|
| `internal/agents` | `Adapter{Name(); Scan(Query, emit)}`，`Register()` | `go test ./e2e -run Claude\|Codex` |
| `internal/price` | `Load()`、`(*Catalog).Cost(model, usage, speed)` | `git tab prices show claude-opus-5-5` |
| `internal/render` | `Render(bill, template, snacks)`、`Apply(msg, art, trailers, cc)`、`Strip()` | `go test ./e2e -run Trailer` |
| `internal/stamp` | `Prepare(args)`、`CommitMsg(file)`、`PostCommit()`、`Preview()` | `go test ./e2e` |
| `internal/install` | `Install(global, files)`、`Uninstall(global, purge)` | `go test ./e2e -run Install` |
| `internal/report` | `Summary()`、`Receipt()`、`Badge()` | `go test ./e2e -run Report` |

加新 agent：在 `internal/agents/` 加一个文件，`init()` 里 `Register(yourAdapter{})`，把那家的本地日志翻成 `Event`。
