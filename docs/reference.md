# git-tab reference

## Commands

| command | what it does |
|---|---|
| `git tab install [--global] [--template T]` | put the hooks in this repository, or every repository |
| `git tab uninstall [--purge]` | take them out; `--purge` also deletes the price cache and this repository's ledger |
| `git tab now [--repo] [--json]` | the receipt the next commit would get |
| `git tab summary [<revs>]` | the bill in a repository's history, by model, agent and month |
| `git tab receipt [<revs>]` | the same, as one long receipt |
| `git tab badge [--svg FILE] [<revs>]` | a "built with $N of AI" badge for a README |
| `git tab plan [add\|rm\|measure\|export]` | what you actually pay for each agent and provider |
| `git tab agents [--no-cloud]` | tell agents on other machines and in cloud sessions to install git-tab |
| `git tab prices update \| show <model>...` | refresh or look up API prices |
| `git tab doctor` | what git-tab sees and what each hook did last |

`summary`, `receipt` and `badge` read receipts back out of `git log`, so they work in any clone, and take any revision range: `git tab summary v1.0..HEAD`, `git tab receipt --since=2026-10-01`.

## Receipt templates

`git config --global tab.template <name>`, or per repository without `--global`.

- **receipt** (the default): a till receipt in the commit body, above the trailers. It shows each model's cost and tokens, the total, what a plan made it cost, what the money would have bought, and the running tab of the branch.
- **standard**: the trailers only.
- **minimal**: one line, `AI-Cost: $5.47 (api-equiv) · paid $0.342 · claude-opus-5-5 · 3 turns`.
- **fun**: the one line, plus `AI-Cost-Equiv: ≈ 1.2 coffees / 1.6 pizza slices`.

Snacks are `name=usd` pairs: `git config --global tab.snacks "flat whites=5.5,bananas=0.3"`.

## Plans: what you actually pay

API prices overstate what a subscriber pays. A plan says how one kind of usage is paid for.

```bash
git tab plan add claude-max 125/mo claude-code --worth 2000   # subscription; a month of it is worth $2,000 at API prices
git tab plan add claude-max 200/mo claude-code                # the same, worth measured from this machine's logs
git tab plan add deepseek api deepseek                        # an API key at list price
git tab plan add reseller x0.5 pi/openrouter                  # a key at half the list price
git tab plan                                                  # your plans, and 30 days of usage by what pays for it
```

- **Terms**: `125/mo`, `1500/yr` (a subscription), `api` (list price), `x0.5` or `50%` (a share of list price).
- **What it pays for**: an agent (`claude-code`), a provider the agent called (`deepseek`, `openrouter`, `opencode-go`) or the model's maker, or an agent and a provider joined by a slash (`pi/anthropic`). The most specific match wins, and a provider or maker match beats an agent-only one, so a `claude-code` subscription does not pay for DeepSeek calls a gateway routes Claude Code to when a `deepseek` plan exists.
- **A subscription's share** is its monthly fee over what a month of its usage is worth at API prices: $125 / $2,000 = 6.25%. Without `--worth`, git-tab measures the worth from the last 30 days of this machine's logs (scaled up when the logs are younger) and measures again weekly, in the background, after a commit. Usage on other devices and on claude.ai is invisible to it, so a stated worth is usually closer.
- Plans live in your global git config (`[tab-plan "claude-max"]`). `GIT_TAB_PLANS`, printed by `git tab plan export`, carries them to a machine without that config.

A receipt with a plan adds `AI-Cost-Paid` and the lines SUBTOTAL, PLAN DISCOUNT and YOU PAID. Usage no plan covers is counted at API prices in what you paid.

## Who pays for which commit

- **An agent commits.** Claude Code, pi and OpenCode pass their session id to the commands they run (`CLAUDE_CODE_SESSION_ID`, `PI_SESSION_ID`, `OPENCODE_SESSION_ID`), and Codex its thread id (`CODEX_THREAD_ID`). git-tab bills only that session and its subagents; other sessions in the same repository keep their usage for their own commits. Any other agent can set `GIT_TAB_SESSION=<agent>:<session id>`.
- **You commit by hand.** Every session whose working directory is inside the repository, since the last receipt, added together.
- **Several work trees**, such as parallel agents under `.claude/worktrees/`: each commit bills the usage inside its own work tree.
- **`git commit --amend`** replaces the receipt with one for the original commit's usage plus everything since.
- **Merges and squashes** get no receipt, because git wrote the message; their usage waits for the next commit. **Rebased** commits keep their receipts. **Cherry-picks and reverts** drop the receipt copied from the original.
- **Another machine's commits**: see [moving a session between machines](install.md#moving-a-session-between-machines).
- **Nothing to bill** means no receipt at all.

## How the price is worked out

- Each API request is priced on its own: uncached input, cache reads, cache writes (Claude's one-hour cache write at twice the input price), output including reasoning. Prompt-size tiers such as gpt-5.5 above 272k tokens apply per request, and Claude's fast mode costs double.
- Prices come from [models.dev](https://models.dev): the model maker's own listing first, then the provider the agent says it called. DeepSeek's peak hours (weekdays 9:00–12:00 and 14:00–18:00 Beijing time) cost twice the off-peak price models.dev lists. A model missing from the table is priced at what the agent itself reported, when it reports a cost.
- Duplicates are dropped: Claude Code's streamed responses are collapsed by message and request id, Codex's running totals are turned back into per-request counts.
- A snapshot of the price table is built into the binary and refreshed weekly in a detached background process after a commit. `git tab prices update` refreshes now.
- The price is fixed when you commit.

## It stays out of the way

- The hooks always exit 0. A panic is caught, and a hook still busy after `tab.timeout` (4 s) is left behind; the commit goes on without a receipt.
- A deleted binary makes the hooks do nothing.
- An empty message still aborts the commit. In the editor, the receipt is added after you save, and only if you wrote something.
- The receipt joins the message's trailer block (`Signed-off-by`, `Co-Authored-By`) and stays above the diff `git commit --verbose` shows.
- `GIT_TAB_DISABLE=1 git commit …` skips it once; `git config tab.enabled false` turns it off for a repository.

## Settings

| git config | default | what it does |
|---|---|---|
| `tab.template` | `receipt` | `receipt`, `standard`, `minimal` or `fun` |
| `tab.snacks` | `coffees=4.5,pizza slices=3.5` | what receipts count money in; the installer sets `杯咖啡=4.5,个煎饼果子=1.4` for a Chinese locale |
| `tab.enabled` | `true` | `false` turns the hooks off where it is set |
| `tab.scope` | `auto` | `repo` bills every session in the repository even when an agent commits |
| `tab.lookback` | `30d` | the oldest usage a repository's first receipt picks up |
| `tab.timeout` | `4s` | how long a hook may run |
| `tab.autoUpdatePrices` | `true` | `false` never refreshes prices in the background |
| `tab-plan.<name>.*` | | plans; use `git tab plan` |

## Trailer format

Schema 1. Every line is a git trailer, so `git interpret-trailers` and `git log --format='%(trailers:key=AI-Cost,valueonly)'` read it.

| key | example | meaning |
|---|---|---|
| `AI-Cost` | `$5.47 (api-equiv)` | USD at public API prices |
| `AI-Cost-Paid` | `$0.342 (6.25% of api-equiv)` | what your plans made it cost; only with a plan |
| `AI-Tokens` | `in=12M cached=11M written=198k out=109k` | every input token, of which `cached` were cache reads and `written` cache writes; output |
| `AI-Model` | `claude-opus-5-5 $4.12, gpt-5.5 $1.35` | priciest first, with a per-model cost when there is more than one |
| `AI-Agent` | `claude-code, codex` | which agents' sessions are on the bill |
| `AI-Turns` | `3` | prompts a person typed; left out when there were none |
| `AI-Session` | `claude-code/4b7a2e61` | the sessions on the bill, by the last 8 characters of their ids |
| `AI-Cost-Equiv` | `≈ 1.2 coffees / 1.6 pizza slices` | `fun` only |
| `AI-Cost-Schema` | `1` | left out by `minimal` and `fun`, where `(api-equiv)` marks schema 1 |

`minimal` and `fun` put the paid amount, top model and turns on the `AI-Cost` line: `$5.47 (api-equiv) · paid $0.342 · claude-opus-5-5 · 3 turns`. A model missing from the price table shows as `AI-Cost: $0.12 (api-equiv; unpriced: some-model)`. A change that breaks parsing will come with `AI-Cost-Schema: 2`.

## Where it looks

| agent | session logs | session id it passes to commands |
|---|---|---|
| Claude Code | `~/.claude/projects`, `~/.config/claude/projects`, `$CLAUDE_CONFIG_DIR` | `CLAUDE_CODE_SESSION_ID` |
| Codex | `~/.codex/sessions`, `~/.codex/archived_sessions`, `$CODEX_HOME` (`.jsonl`, `.jsonl.zst`) | `CODEX_THREAD_ID` |
| pi | `~/.pi/agent/sessions`, `$PI_CODING_AGENT_DIR`, `$PI_CODING_AGENT_SESSION_DIR` | `PI_SESSION_ID` |
| OpenCode | `~/.local/share/opencode/opencode.db` | `OPENCODE_SESSION_ID` |
| DeepSeek Harness | DeepSeek Harness's local session store | `GIT_TAB_SESSION=dsh:<id>` |

git-tab only reads them. Nothing leaves your machine except the weekly price download.

## Teach it another agent

Each agent is one Go file in [`internal/agents/`](../internal/agents) that turns the tool's local logs into events:

```go
type Adapter interface {
	Name() string        // what AI-Agent says
	Roots() []string     // where its logs live, for `git tab doctor`
	Scan(q Query, emit func(Event)) error
}

func init() { Register(myAgent{}) }
```

`Scan` emits one `Event` per API request (model, provider, token usage, working directory, time) and one per prompt a person typed. Pricing, plans, attribution and the receipt come for free. Add a fixture under [`e2e/`](../e2e) and run `go test ./e2e`. [`design.md`](design.md) (in Chinese) explains the design.

## Known gaps

- Codex's priority tier, which costs double, is billed at the standard price.
- A session started in a parent directory that commits to two repositories inside it can have the parent directory's usage counted in both.
- GitHub's "Squash and merge" button runs no local hook, so the squashed commit has no receipt.
- DeepSeek's peak-hour rule ignores Chinese public holidays.
- A Claude Code session routed through a gateway to another provider's model is priced as the model its log names.
