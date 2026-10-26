# git-tab

**Every commit an AI helped write comes with the bill.**

git-tab is a git hook. When you commit, by hand or through Claude Code or Codex, it reads the agent's session logs on your machine, works out what that work would have cost at public API prices, and writes the amount into the commit message:

```
$ git log -1
commit 8bc2ada0b51c…
Author: you <you@example.com>

    feat: parse codex rollouts

    AI-Cost: $1.43 (api-equiv)
    AI-Tokens: in=1.9M cached=1.8M written=90k out=31k
    AI-Model: claude-opus-5-5
    AI-Agent: claude-code
    AI-Turns: 1
    AI-Cost-Schema: 1
```

The receipt is ordinary text in the history, so it goes wherever the repository goes. Three years from now, `git log` on a new laptop still says what that refactor cost. There is no server, no account and no API key, and git-tab will never stop a commit.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh
```

That puts one binary in `~/.local/bin` and turns git-tab on for every repository on the machine. It uses git's config-based hooks (Git 2.54 and later), so it runs next to husky, lefthook, pre-commit and whatever is already in `.git/hooks`, and it leaves `core.hooksPath` alone.

With Go instead: `go install github.com/fish2lab/git-tab@latest && git tab install --global`.

On git older than 2.54, run `git tab install` inside each repository. It writes hook files there, and a hook that was already in place keeps running after git-tab's.

## Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh -s -- --uninstall
```

This removes the hooks, the price cache and the binary. Commits you already made keep their receipts: they are text.

## Four kinds of receipt

Pick one with `git config --global tab.template <name>`, or per repository without `--global`.

**standard** (the default) is shown above: one trailer per fact, easy to grep and to parse.

**minimal** is one line:

```
AI-Cost: $9.50 (api-equiv) · claude-opus-5-5 · 1 turn
```

**receipt** prints a till receipt in the commit body, above the standard trailers. This one is real: the session that wrote git-tab, printed partway through. One prompt from a person, 64 API calls.

```
 ____________________________________
|                                    |
|         A I   R E C E I P T        |
|     git-tab · USD at API prices    |
|        2026-10-26 15:34 +08        |
|                                    |
| claude-opus-5-5              $9.50 |
|   in 15M  cached 14M               |
|   out 184k  written 368k  x64      |
| - - - - - - - - - - - - - - - - -  |
| TOTAL                        $9.50 |
| 1 turn · 64 calls · 34m busy       |
| ≈ 2.1 杯咖啡 / 6.8 个煎饼果子      |
|                                    |
|    || || | | |||  |  || | | ||     |
|        THANK YOU, COME AGAIN       |
|____________________________________|
 \/\/\/\/\/\/\/\/\/\/\/\/\/\/\/\/\/\/

AI-Cost: $9.50 (api-equiv)
AI-Tokens: in=15M cached=14M written=368k out=184k
AI-Model: claude-opus-5-5
AI-Agent: claude-code
AI-Turns: 1
AI-Cost-Schema: 1
```

**fun** is the one-liner plus what the money would have bought:

```
AI-Cost: $9.50 (api-equiv) · claude-opus-5-5 · 1 turn
AI-Cost-Equiv: ≈ 2.1 杯咖啡 / 6.8 个煎饼果子
```

The default exchange rate is $4.50 for a cup of coffee and $1.40 for a jianbing (煎饼果子, a Tianjin street crêpe). Bring your own: `git config --global tab.snacks "flat whites=5.5,bananas=0.3"`.

## See the bill before you commit

```
$ git tab now
next commit in coding-rec settles only claude-code:a6f8f6b4-3d0c-4fac-983f-7328bc659162 (the session running this command)
  usage since 2026-09-26 15:34 · 64 calls · 1 turn · 1 session · template standard

  AI-Cost: $9.50 (api-equiv)
  AI-Tokens: in=15M cached=14M written=368k out=184k
  AI-Model: claude-opus-5-5
  AI-Agent: claude-code
  AI-Turns: 1
  AI-Cost-Schema: 1
```

`--json` prints the bill for scripts; `--repo` shows what a commit made by hand would settle.

## Add up a whole project

Everything below reads the receipts back out of `git log`, so it works on any clone, including ones made on machines without git-tab.

```
$ git tab summary
git tab summary · my-project
  3 of 4 commits carry a receipt · 2026-10-26 → 2026-10-26

  total                   $3.32  (USD at API prices)
  tokens             in 3.8M · cached 3.5M · written 150k · out 56k
  turns              3
  average            $1.11 per commit

  by model
    gpt-5.5                       $1.54   46%  ███████████
    claude-opus-5-5               $1.43   43%  ██████████
    claude-sonnet-5-5            $0.310    9%  ██
    claude-haiku-5-5             $0.038    1%  █

  by agent
    claude-code                   $1.78   54%  █████████████
    codex                         $1.54   46%  ███████████

  priciest commits
         $1.54  da88b4deb  2026-10-26  test: codex fixtures
         $1.43  8bc2ada0b  2026-10-26  feat: parse codex rollouts
        $0.348  aef9e2431  2026-10-26  fix: dedup streamed lines
```

`git tab receipt` prints the same as one long receipt, signed "KEEP FOR YOUR RECORDS". Both take a revision range: `git tab summary v1.0..HEAD`, `git tab receipt --since=2026-10-01`.

`git tab badge` prints a badge for your README, and `git tab badge --svg badge.svg` writes it as a file:

![Built with $3.32 of AI](https://img.shields.io/badge/built_with_%243.32_of_AI-8A2BE2)

## Who pays for which commit

- **An agent commits.** Claude Code passes `CLAUDE_CODE_SESSION_ID` to the commands it runs, and Codex passes `CODEX_THREAD_ID`. git-tab reads it and bills only that session and its subagents. Other sessions working in the same repository at the same time keep their usage for their own commits. No plugin is needed. Any other agent can do the same by setting `GIT_TAB_SESSION=<agent>:<session id>`.
- **You commit by hand.** Every session whose working directory is inside the repository, since the last receipt, added together. Two Claude Code windows and a Codex run on one feature make one bill.
- **Several work trees**, such as agents working in parallel under `.claude/worktrees/`: each commit bills the usage inside its own work tree, and leaves the others' usage for their own commits.
- **`git commit --amend`** replaces the receipt with a cumulative one: what the original commit cost plus everything since.
- **Merges and squashes** get no receipt, because git wrote those messages; their usage waits for the next ordinary commit. **Rebased** commits keep the receipts they had. **Cherry-picks and reverts** drop the receipt copied from the original commit.
- **Nothing to bill**, because no agent touched the repository since the last commit, means no trailers at all.

The record of which usage is already on a receipt lives in `.git/git-tab/ledger.json`, on your machine only.

## How the price is worked out

- Each API request is priced on its own, then the requests are added up. Cache reads and cache writes have their own prices. Claude's one-hour cache write costs twice the input price. Prompt-size tiers, such as gpt-5.5 above 272k tokens, apply per request. Claude's fast mode costs double.
- Duplicates are dropped. Claude Code writes one streamed response as several log lines, which git-tab collapses by message id and request id, as ccusage and magpie do. Codex logs running totals; git-tab uses each request's own count and skips repeats and the checkpoints a forked thread replays.
- `(api-equiv)` marks the amount as what the same tokens would cost through the public API. On a Claude Max or ChatGPT Pro subscription you paid a flat fee instead.
- Prices come from [models.dev](https://models.dev). A snapshot is built into the binary. Once a week, after a commit, git-tab refreshes it in a detached background process, so the hook itself never waits on the network. `git tab prices update` refreshes now; `git tab prices show` says what git-tab will charge:

```
$ git tab prices show claude-opus-5-5 gpt-5.5
prices: built-in catalog from https://models.dev/api.json, fetched 2026-10-26
  claude-opus-5-5: input $4 · output $20 · cache read $0.2 · cache write $5 (1h $8) per MTok
  gpt-5.5: input $5 · output $30 · cache read $0.5 · cache write $5 per MTok
    over 272k prompt tokens: input $10 · output $45 · cache read $1 · cache write $10
```

- The price is fixed when you commit, so the receipt keeps showing what the work cost that day, whatever prices do later.

## It stays out of your way

- The hooks always exit 0. A crash inside git-tab is caught, and a hook still busy after 4 seconds (`tab.timeout`) is abandoned; the commit goes through without a receipt.
- If the binary is deleted, the hooks quietly do nothing.
- A commit takes about 45 ms longer: a median of 61 ms against 16 ms without git-tab, measured over 20 commits on a MacBook Air M5 with 178 Claude Code session files on disk.
- An empty message still aborts the commit. When you write the message in the editor, the receipt is added after you save, and only if you wrote something.
- The receipt joins the trailer block your message already has (`Signed-off-by`, `Co-Authored-By`), and always stays above the diff that `git commit --verbose` shows.
- `git tab doctor` shows what git-tab can see and what each hook did last time.
- `GIT_TAB_DISABLE=1 git commit …` skips it once; `git config tab.enabled false` turns it off for a repository.

## Settings

| git config | default | what it does |
|---|---|---|
| `tab.template` | `standard` | `minimal`, `standard`, `receipt` or `fun` |
| `tab.snacks` | `杯咖啡=4.5,个煎饼果子=1.4` | what `fun` and `receipt` count the money in: `name=usd,name=usd` |
| `tab.enabled` | `true` | `false` turns the hooks off where it is set |
| `tab.scope` | `auto` | `repo` bills every session in the repository even when an agent commits |
| `tab.lookback` | `30d` | the oldest usage a repository's first receipt will pick up |
| `tab.timeout` | `4s` | how long a hook may run before the commit goes on without it |
| `tab.autoUpdatePrices` | `true` | `false` never refreshes prices in the background |

## The trailer format

This is schema 1. Every line is a standard git trailer, so `git interpret-trailers` reads it, and so does `git log --format='%(trailers:key=AI-Cost,valueonly)'`.

| key | example | meaning |
|---|---|---|
| `AI-Cost` | `$1.43 (api-equiv)` | USD at public API prices; `minimal` and `fun` add `· <top model> [+N] · <N> turns` |
| `AI-Tokens` | `in=1.9M cached=1.8M written=90k out=31k` | all input tokens, of which `cached` were cache reads and `written` cache writes; output |
| `AI-Model` | `claude-sonnet-5-5 $0.310, claude-haiku-5-5 $0.038` | priciest first, with a per-model cost when there is more than one |
| `AI-Agent` | `claude-code, codex` | which agents' sessions are on the bill |
| `AI-Turns` | `3` | prompts a person typed; left out when there were none |
| `AI-Cost-Equiv` | `≈ 2.1 杯咖啡 / 6.8 个煎饼果子` | `fun` only |
| `AI-Cost-Schema` | `1` | left out by `minimal` and `fun`, where `(api-equiv)` marks schema 1 |

A model missing from the price table shows up as `AI-Cost: $0.12 (api-equiv; unpriced: some-model)`. Any change that breaks parsing will come with `AI-Cost-Schema: 2`.

## Where it looks

| agent | session logs |
|---|---|
| Claude Code | `~/.claude/projects/**/*.jsonl`, `~/.config/claude/projects`, or `$CLAUDE_CONFIG_DIR` |
| Codex | `~/.codex/sessions` and `~/.codex/archived_sessions` (or `$CODEX_HOME`): `rollout-*.jsonl` and `.jsonl.zst` |

git-tab only reads them, and nothing leaves your machine except the weekly price download from models.dev.

## Teach it another agent

OpenCode, pi, Cursor, DeepSeek's harness: each adapter is one Go file in [`internal/agents/`](internal/agents) that turns that tool's local logs into events.

```go
type Adapter interface {
	Name() string        // "opencode": what AI-Agent says
	Roots() []string     // where its logs live, for `git tab doctor`
	Scan(q Query, emit func(Event)) error
}

func init() { Register(openCode{}) }
```

`Scan` emits one `Event` per API request (model, token usage, working directory, time) and one per prompt a person typed, for everything between `q.Since` and `q.Until`. Pricing, attribution and the receipt come for free. Add a fixture to [`e2e/e2e_test.go`](e2e/e2e_test.go) and run `go test ./e2e`. [`docs/design.md`](docs/design.md) (in Chinese) explains the rest of the design.

## Known gaps

- Codex's priority processing tier, which costs double, is billed at the standard price.
- A Claude Code session started in a parent directory that commits to two repositories inside it can have that parent directory's usage counted in both.
- GitHub's "Squash and merge" button runs no local hook, so the squashed commit on `main` has no receipt, and the branch commits that had one are gone from `main`'s history.
- Claude Code's `<synthetic>` messages and API error responses are not billed, since no tokens were charged for them.

## Why "tab"

As in "put it on my tab". Other names considered: git-chit, tokentab, git-cost, xiaopiao (小票, Chinese for a till receipt). "receipt" was already taken by a project with the same idea.

## Thanks

git-tab copies its log parsing from [magpie](https://github.com/yetone/magpie), its prices from [models.dev](https://models.dev), its trailer placement rules from trusty-mpm's [`commit_trailers.rs`](https://github.com/bobmatnyc/trusty-tools/blob/main/crates/trusty-mpm/src/core/commit_trailers.rs), and the `AI-Cost` key from [Scopeon](https://github.com/sorunokoe/Scopeon). [ccusage](https://github.com/ryoppippi/ccusage) showed how to drop Claude Code's duplicate lines.

## License

MIT
