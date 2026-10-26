---
name: git-tab
description: Read, explain and set up git-tab, which puts an AI cost receipt (tokens, models, API-equivalent USD, what the user actually paid) on every git commit. Use when the user asks what AI work on a repository cost, what a commit's receipt or its AI-Cost / AI-Cost-Paid / AI-Session trailers mean, wants receipts on their commits, moves work to another machine or a cloud session, or says how they pay for a model (a subscription such as Claude Max or ChatGPT Pro, an API key at list price, a reseller discount).
---

# git-tab

git-tab is a git hook. When a commit is made it reads the local logs of
Claude Code, Codex, pi, OpenCode and DeepSeek Harness, prices the usage
since the last commit at API rates, and writes a receipt into the commit
message. Nothing it does can fail a commit.

## Is it installed?

Run `git tab doctor`. If the command is missing:

```sh
curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh
```

The installer puts the hooks in every repository on Git 2.54 or later. On
older git, also run `git tab install` inside the repository. Ask before
installing: it changes the user's git config.

## Reading receipts

- `git tab now`: the receipt the next commit would get.
- `git tab summary`: the repository's whole bill, by model, agent and month.
- `git tab receipt`: the same, as a paper receipt.
- `AI-Cost` is the API-equivalent price. `AI-Cost-Paid` appears when a plan
  pays for the usage and is what the user actually paid. `AI-Session` names
  the sessions the receipt covers, so another machine never bills them twice.
- Never remove a receipt or its `AI-*` trailers from a commit message,
  including when you rewrite or squash commits; a squash may merge them.

## What the user actually pays

API prices overstate what a subscriber pays. Run `git tab plan`: it lists
the user's plans and the last 30 days of usage by what pays for it. For each
row marked "no plan" that costs more than a few dollars, ask the user once,
in one question, how they pay for it, then record the answer:

| The user says | Run |
| --- | --- |
| Claude Max, $125 a month, worth about $2,000 of API usage a month | `git tab plan add claude-max 125/mo claude-code --worth 2000` |
| Claude Max, $200 a month, no idea of the worth | `git tab plan add claude-max 200/mo claude-code` (measured from this machine's logs) |
| DeepSeek's own API key | `git tab plan add deepseek api deepseek` |
| A reseller at half the list price | `git tab plan add reseller x0.5 pi/openrouter` |

The last argument says what the plan pays for: an agent (`claude-code`), a
provider (`deepseek`) or both (`pi/anthropic`); copy it from the row in
`git tab plan`. A subscription used on other devices or on claude.ai is worth
more than this machine's logs show, so prefer the user's own estimate
(`--worth`) when they have one. Plans live in the user's global git config;
`git tab plan rm <name>` removes one.

## Other machines and cloud sessions

`git tab agents` adds one line to the repository's AGENTS.md or CLAUDE.md
telling agents on any machine to install git-tab before committing, and a
SessionStart hook to `.claude/settings.json` that installs it in Claude Code
cloud sessions. Commit both files. For cloud receipts at the user's plan
prices, `git tab plan export` prints a `GIT_TAB_PLANS=...` line to add to
the cloud environment's variables.

Receipts are plain text in the commits, so they travel with the code. Each
clone keeps its own record of what it has billed, and a session that a
commit from another machine already billed is not billed again.
