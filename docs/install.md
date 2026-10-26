# Installing git-tab

## One machine

```bash
curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh
```

The script puts one binary in `~/.local/bin` (`GIT_TAB_BIN_DIR` picks another place) and turns git-tab on for every repository through git's config-based hooks, which need Git 2.54 or later. Config hooks run next to husky, lefthook, pre-commit and whatever is in `.git/hooks`, and git-tab leaves `core.hooksPath` alone.

On an older git the script says so, and you run `git tab install` inside each repository instead. That writes hook files into the repository's hooks directory; a hook that was already there is kept and runs after git-tab's.

With Go 1.26 or later: `go install github.com/fish2lab/git-tab@latest && git tab install --global`.

On a machine whose locale is Chinese, the installer also sets `tab.snacks` so receipts count money in 杯咖啡 and 个煎饼果子. Everywhere else it is coffees and pizza slices.

## Uninstall

```bash
curl -fsSL https://raw.githubusercontent.com/fish2lab/git-tab/main/install.sh | sh -s -- --uninstall
```

This removes the hooks (global and the current repository), the price cache, the current repository's ledger and the binary. Commits keep their receipts: they are text. Your plans stay in `~/.gitconfig` until you remove them with `git tab plan rm <name>`.

## The Claude Code plugin

```
/plugin install git-tab --marketplace fish2lab/git-tab
```

The plugin adds a skill; the binary comes from the installer above. With the skill, Claude Code knows what a receipt says, installs git-tab when it is missing (after asking), and asks you once how you pay for each model so receipts can show what you actually paid. The skill is a plain [`SKILL.md`](../skills/git-tab/SKILL.md); agents that read Agent Skills, such as Codex, pi and OpenCode, can use the same file.

## Other machines and cloud sessions

Receipts live in commit messages, so they travel with every clone, push and pull. Each machine needs the hook to add new ones. One command per repository arranges that:

```bash
git tab agents
```

- It adds one line to the repository's `AGENTS.md`, or to its `CLAUDE.md` when that is what Claude Code reads there. Any agent that opens the repository on a new machine is told to install git-tab before its first commit.
- It adds a `SessionStart` hook to `.claude/settings.json` that installs git-tab when a Claude Code cloud session starts (`CLAUDE_CODE_REMOTE=true`). On the cloud VM (Ubuntu 24.04, whose packaged git is 2.43) `git tab install` sees no config hooks and writes hook files. `--no-cloud` skips this.

Commit both files. Claude Code cloud sessions run the hooks a repository commits; they load no plugins.

Your plans are in your own git config, which a cloud VM does not have. `git tab plan export` prints them as one variable:

```
GIT_TAB_PLANS=claude-max=125/mo@2000:claude-code;deepseek=api:deepseek
```

Add it to the cloud environment's variables and cloud receipts show what you paid, too.

### Moving a session between machines

Each clone keeps its own record of which usage is on a receipt (`.git/git-tab/ledger.json`). Two things keep a session from being billed twice when its log moves with you, such as `claude --teleport` pulling a cloud session's conversation onto your laptop:

1. A clone's first receipt only counts usage after the commit it is built on.
2. Every receipt names its sessions (`AI-Session: claude-code/4b7a2e61`). A clone treats a session named by a commit it did not make as settled up to that commit's time.

The running tab on each receipt adds up every receipt in the branch's history, wherever it was made.

## Checking

`git tab doctor` shows the hooks, the session logs git-tab can read, the price catalog, the ledger and what each hook did the last time it ran.
