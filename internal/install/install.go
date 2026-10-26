// Package install puts git-tab's hooks in place and takes them out again.
//
// With Git 2.54 or later the hooks are git config entries
// (hook.<name>.command), globally or for one repository; they run next to
// a repository's own hooks without touching them. Older git gets hook
// files in one repository, and a hook that was already there is kept
// under <name>.pre-git-tab and run after ours.
package install

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/fish2lab/git-tab/internal/gitx"
	"github.com/fish2lab/git-tab/internal/ledger"
	"github.com/fish2lab/git-tab/internal/price"
	"github.com/fish2lab/git-tab/internal/render"
)

var Events = []string{"prepare-commit-msg", "commit-msg", "post-commit"}

const (
	marker = "# Installed by git-tab"
	saved  = ".pre-git-tab"
)

func name(event string) string { return "git-tab-" + event }

// Binary is the path the hooks call: the git-tab on PATH when it is this
// program (a stable symlink, for package managers), else this program.
func Binary() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "git-tab"
	}
	if p, err := exec.LookPath("git-tab"); err == nil {
		a, _ := filepath.EvalSymlinks(p)
		b, _ := filepath.EvalSymlinks(exe)
		if a == b {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
		}
	}
	return exe
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// command is a config hook's shell line. git appends "$@", the hook's
// arguments. A missing binary makes it do nothing, never fail.
func command(bin, event string) string {
	return fmt.Sprintf(`b=%s; [ -x "$b" ] || b=$(command -v git-tab) || exit 0; exec "$b" hook %s`, quote(bin), event)
}

func script(bin, event string) string {
	return fmt.Sprintf(`#!/bin/sh
%s: `+"`git tab uninstall`"+` puts back what was here.
b=%s
[ -x "$b" ] || b=$(command -v git-tab 2>/dev/null)
[ -n "$b" ] && "$b" hook %s "$@" </dev/null
prev="$0%s"
[ -x "$prev" ] && exec "$prev" "$@"
exit 0
`, marker, quote(bin), event, saved)
}

type Options struct {
	Global   bool
	Files    bool   // hook files even where config hooks work
	Template string // also set tab.template
	Dir      string // the repository, for a local install
}

// Install returns what it did, one line each.
func Install(o Options) ([]string, error) {
	bin := Binary()
	var did []string
	scope := "--local"
	if o.Global {
		scope = "--global"
	}
	if o.Global || !o.Files && gitx.ConfigHooks() {
		if !gitx.ConfigHooks() {
			a, b := gitx.Version()
			return nil, fmt.Errorf("a global install needs Git 2.54 or later (config-defined hooks); this is %d.%d. Upgrade git, or run `git tab install` in each repository", a, b)
		}
		if !o.Global {
			if _, err := gitx.Open(o.Dir); err != nil {
				return nil, errors.New("not in a git repository (use --global for every repository)")
			}
		}
		for _, ev := range Events {
			n := name(ev)
			if _, err := gitx.Run(o.Dir, "config", scope, "--replace-all", "hook."+n+".command", command(bin, ev)); err != nil {
				return did, err
			}
			if _, err := gitx.Run(o.Dir, "config", scope, "--replace-all", "hook."+n+".event", ev); err != nil {
				return did, err
			}
		}
		did = append(did, fmt.Sprintf("hooks %s in %s git config → %s", strings.Join(Events, ", "), strings.TrimPrefix(scope, "--"), bin))
	} else {
		repo, err := gitx.Open(o.Dir)
		if err != nil {
			return nil, errors.New("not in a git repository")
		}
		dir, err := hooksDir(repo)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		for _, ev := range Events {
			path := filepath.Join(dir, ev)
			if b, err := os.ReadFile(path); err == nil && !strings.Contains(string(b), marker) {
				if _, err := os.Stat(path + saved); err == nil {
					return did, fmt.Errorf("%s and %s both exist; move one away first", path, path+saved)
				}
				if err := os.Rename(path, path+saved); err != nil {
					return did, err
				}
				did = append(did, fmt.Sprintf("kept your %s hook as %s; it runs after git-tab", ev, filepath.Base(path+saved)))
			}
			if err := os.WriteFile(path, []byte(script(bin, ev)), 0o755); err != nil {
				return did, err
			}
		}
		did = append(did, fmt.Sprintf("hook files %s in %s → %s", strings.Join(Events, ", "), dir, bin))
	}
	if _, err := gitx.Run(o.Dir, "config", "tab.snacks"); err != nil && chinese() {
		if _, err := gitx.Run(o.Dir, "config", scope, "tab.snacks", render.ChineseSnacks); err == nil {
			did = append(did, fmt.Sprintf("tab.snacks = %s (%s)", render.ChineseSnacks, strings.TrimPrefix(scope, "--")))
		}
	}
	if o.Template != "" {
		if _, err := gitx.Run(o.Dir, "config", scope, "tab.template", o.Template); err != nil {
			return did, err
		}
		did = append(did, fmt.Sprintf("tab.template = %s (%s)", o.Template, strings.TrimPrefix(scope, "--")))
	}
	return did, nil
}

// chinese says the person's locale is Chinese, by language or by region,
// so that receipts count money in 煎饼果子.
func chinese() bool {
	var locales []string
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" && v != "C" && v != "POSIX" {
			locales = append(locales, v)
		}
	}
	// macOS terminals often set no locale; the system settings have one.
	if len(locales) == 0 && runtime.GOOS == "darwin" {
		if out, err := exec.Command("defaults", "read", "-g", "AppleLocale").Output(); err == nil {
			locales = append(locales, strings.TrimSpace(string(out)))
		}
	}
	for _, l := range locales {
		l, _, _ = strings.Cut(l, ".")
		lang, region, _ := strings.Cut(strings.ReplaceAll(l, "-", "_"), "_")
		if strings.EqualFold(lang, "zh") {
			return true
		}
		for _, r := range []string{"CN", "TW", "HK", "MO", "SG"} {
			if strings.EqualFold(region, r) || strings.HasSuffix(strings.ToUpper(region), "_"+r) {
				return true
			}
		}
	}
	return false
}

func hooksDir(repo *gitx.Repo) (string, error) {
	dir, err := gitx.Run(repo.Top, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repo.Top, dir)
	}
	return dir, nil
}

// Uninstall takes out the global hooks and the current repository's, and
// with purge every price cache and this repository's ledger.
func Uninstall(dir string, purge bool) []string {
	var did []string
	removeSections := func(scope string) bool {
		removed := false
		for _, ev := range Events {
			if _, err := gitx.Run(dir, "config", scope, "--remove-section", "hook."+name(ev)); err == nil {
				removed = true
			}
		}
		return removed
	}
	if removeSections("--global") {
		did = append(did, "removed the hooks from global git config")
	}
	if repo, err := gitx.Open(dir); err == nil {
		if removeSections("--local") {
			did = append(did, "removed the hooks from this repository's git config")
		}
		if hd, err := hooksDir(repo); err == nil {
			for _, ev := range Events {
				path := filepath.Join(hd, ev)
				b, err := os.ReadFile(path)
				if err != nil || !strings.Contains(string(b), marker) {
					continue
				}
				os.Remove(path)
				if _, err := os.Stat(path + saved); err == nil {
					os.Rename(path+saved, path)
					did = append(did, "restored your "+ev+" hook")
				}
				did = append(did, "removed hook file "+path)
			}
		}
		if purge {
			if err := os.RemoveAll(ledger.Dir(repo.CommonDir)); err == nil {
				did = append(did, "deleted this repository's ledger")
			}
		}
	}
	if purge {
		if err := os.RemoveAll(filepath.Dir(price.CachePath())); err == nil {
			did = append(did, "deleted the price cache")
		}
	}
	return did
}

// Status describes where the hooks are installed, for `git tab doctor`.
func Status(dir string) []string {
	var out []string
	if gitx.ConfigHooks() {
		for _, ev := range Events {
			list, _ := gitx.Run(dir, "hook", "list", "--show-scope", ev)
			for _, l := range strings.Split(list, "\n") {
				if strings.Contains(l, name(ev)) {
					out = append(out, ev+": "+strings.Replace(strings.TrimSpace(l), "\t", " config: ", 1))
				}
			}
		}
	}
	if repo, err := gitx.Open(dir); err == nil {
		if hd, err := hooksDir(repo); err == nil {
			for _, ev := range Events {
				if b, err := os.ReadFile(filepath.Join(hd, ev)); err == nil && strings.Contains(string(b), marker) {
					out = append(out, ev+": hook file "+filepath.Join(hd, ev))
				}
			}
		}
	}
	return out
}
