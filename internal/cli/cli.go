// Package cli is git-tab's command line.
package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fish2lab/git-tab/internal/agents"
	"github.com/fish2lab/git-tab/internal/gitx"
	"github.com/fish2lab/git-tab/internal/install"
	"github.com/fish2lab/git-tab/internal/ledger"
	"github.com/fish2lab/git-tab/internal/price"
	"github.com/fish2lab/git-tab/internal/render"
	"github.com/fish2lab/git-tab/internal/report"
	"github.com/fish2lab/git-tab/internal/stamp"
)

// Version is set at build time.
var Version = "dev"

const usage = `git tab — a receipt for every commit an AI helped write

  git tab install [--global] [--template T]   put the hooks in (this repo, or every repo)
  git tab uninstall [--purge]                 take them out again (global and this repo)
  git tab now [--repo] [--json]               what the next commit's receipt would say
  git tab summary [<revs>]                    the project's AI bill, from its history
  git tab receipt [<revs>]                    the same, printed as a receipt
  git tab badge [--svg FILE] [<revs>]         a "built with $N of AI" README badge
  git tab prices update | show <model>...     refresh or look up API prices (models.dev)
  git tab plan [add|rm|measure|export]        what you actually pay: subscriptions and discounted keys
  git tab agents [--no-cloud]                 tell agents on other machines and cloud boxes to install it
  git tab doctor                              what git-tab sees and did last

Templates (git config tab.template): receipt (default), standard, minimal, fun.
Settings: tab.enabled, tab.snacks, tab.scope (auto|repo), tab.lookback, tab.timeout, tab.autoUpdatePrices.
`

func Run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "hook":
		return hook(rest)
	case "install":
		return installCmd(rest)
	case "uninstall":
		return uninstallCmd(rest)
	case "now", "preview":
		return nowCmd(rest)
	case "summary":
		return summaryCmd(rest)
	case "receipt":
		return receiptCmd(rest)
	case "badge":
		return badgeCmd(rest)
	case "prices":
		return pricesCmd(rest)
	case "plan", "plans":
		return planCmd(rest)
	case "agents":
		return agentsCmd(rest)
	case "doctor":
		return doctorCmd()
	case "version", "--version", "-v":
		fmt.Println("git-tab", Version)
		return 0
	case "help", "--help", "-h":
		fmt.Print(usage)
		return 0
	}
	fmt.Fprintf(os.Stderr, "git tab: unknown command %q\n\n%s", cmd, usage)
	return 2
}

// hook runs a git hook. It always succeeds: a receipt is never worth a
// failed commit.
func hook(args []string) int {
	if len(args) == 0 {
		return 0
	}
	event, rest := args[0], args[1:]
	if stamp.Idle(event, rest) {
		return 0
	}
	timeout := stamp.Timeout()
	switch event {
	case "prepare-commit-msg":
		stamp.Guard(timeout, func() { stamp.Prepare(rest) })
	case "commit-msg":
		stamp.Guard(timeout, func() { stamp.CommitMsg(rest) })
	case "post-commit":
		stamp.Guard(timeout, stamp.PostCommit)
	}
	return 0
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("git tab "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func installCmd(args []string) int {
	fs := newFlags("install")
	global := fs.Bool("global", false, "install for every repository (Git 2.54+)")
	files := fs.Bool("files", false, "write hook files instead of config hooks")
	tmpl := fs.String("template", "", "set tab.template: "+strings.Join(render.Templates, ", "))
	if fs.Parse(args) != nil {
		return 2
	}
	if *tmpl != "" && !contains(render.Templates, *tmpl) {
		fmt.Fprintf(os.Stderr, "git tab: unknown template %q (one of %s)\n", *tmpl, strings.Join(render.Templates, ", "))
		return 2
	}
	did, err := install.Install(install.Options{Global: *global, Files: *files, Template: *tmpl})
	for _, d := range did {
		fmt.Println("✓", d)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "git tab:", err)
		return 1
	}
	fmt.Println("  every commit now gets a receipt; `git tab now` shows the next one")
	return 0
}

func uninstallCmd(args []string) int {
	fs := newFlags("uninstall")
	purge := fs.Bool("purge", false, "also delete the price cache and this repository's ledger")
	if fs.Parse(args) != nil {
		return 2
	}
	did := install.Uninstall("", *purge)
	if len(did) == 0 {
		fmt.Println("nothing to remove")
	}
	for _, d := range did {
		fmt.Println("✓", d)
	}
	return 0
}

func nowCmd(args []string) int {
	fs := newFlags("now")
	repoScope := fs.Bool("repo", false, "count every session in this repository, even when run by an agent")
	asJSON := fs.Bool("json", false, "print the bill as JSON")
	if fs.Parse(args) != nil {
		return 2
	}
	pv, err := stamp.Next(*repoScope)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git tab:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(map[string]any{"bill": pv.Bill, "since": pv.Window.State.Earliest(), "scope": pv.Window.Scope.String(), "trailers": pv.Trailers})
		return 0
	}
	b := pv.Bill
	scope := "every session in this repository"
	if pv.Window.Scope != nil {
		scope = "only " + pv.Window.Scope.String() + " (the session running this command)"
	}
	fmt.Printf("next commit in %s settles %s\n", filepath.Base(pv.Repo.Top), scope)
	if b.Empty() {
		fmt.Printf("  no unsettled agent usage since %s — the commit would get no receipt\n", since(pv.Window))
		return 0
	}
	fmt.Printf("  usage since %s · %s · %s · %s · template %s\n\n",
		since(pv.Window), count(b.Requests, "call"), count(b.Turns, "turn"), count(b.Sessions, "session"), pv.Settings.Template)
	for _, l := range pv.Art {
		fmt.Println("  " + l)
	}
	if len(pv.Art) > 0 {
		fmt.Println()
	}
	for _, t := range pv.Trailers {
		fmt.Println("  " + t)
	}
	return 0
}

func since(w stamp.Window) string {
	t := w.State.Earliest()
	if t.Before(w.Floor) {
		t = w.Floor
	}
	if t.IsZero() {
		return "the start"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func history(args []string) (*report.History, bool) {
	h, err := report.Load("", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git tab:", err)
		return nil, false
	}
	return h, true
}

func summaryCmd(args []string) int {
	h, ok := history(args)
	if !ok {
		return 1
	}
	fmt.Print(report.Summary(h))
	return 0
}

func snacks() []render.Snack {
	v, err := gitx.Run("", "config", "tab.snacks")
	if err != nil {
		v = render.DefaultSnacks
	}
	return render.ParseSnacks(v)
}

func receiptCmd(args []string) int {
	h, ok := history(args)
	if !ok {
		return 1
	}
	fmt.Print(report.Receipt(h, snacks()))
	return 0
}

func badgeCmd(args []string) int {
	fs := newFlags("badge")
	svg := fs.String("svg", "", "write the badge as an SVG file instead")
	if fs.Parse(args) != nil {
		return 2
	}
	h, ok := history(fs.Args())
	if !ok {
		return 1
	}
	cost := h.Totals().Cost
	if *svg != "" {
		if err := os.WriteFile(*svg, []byte(report.BadgeSVG(cost)), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "git tab:", err)
			return 1
		}
		fmt.Printf("wrote %s; in a README: ![Built with %s of AI](%s)\n", *svg, report.BadgeText(cost), *svg)
		return 0
	}
	fmt.Printf("![Built with %s of AI](%s)\n", report.BadgeText(cost), report.BadgeURL(cost))
	return 0
}

func pricesCmd(args []string) int {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "update":
		quiet := len(args) > 1 && args[1] == "--quiet"
		c, err := price.Update(20 * time.Second)
		if err != nil {
			if !quiet {
				fmt.Fprintln(os.Stderr, "git tab: price update failed:", err)
			}
			return 1
		}
		if !quiet {
			n := 0
			for _, p := range c.Providers {
				n += len(p)
			}
			fmt.Printf("✓ %d priced models from %s → %s\n", n, c.Source, price.CachePath())
		}
		return 0
	case "show":
		c := price.Load()
		fmt.Printf("prices: %s catalog from %s, fetched %s\n", c.From, c.Source, c.Fetched.Local().Format("2006-01-02"))
		for _, m := range args[1:] {
			p, from, ok := c.Lookup(m, "")
			if !ok {
				fmt.Printf("  %s: unknown\n", m)
				continue
			}
			fmt.Printf("  %s (%s): %s per MTok\n", m, from, rates(p.Flat))
			if from == "deepseek" {
				fmt.Println("    twice that on weekdays 9:00-12:00 and 14:00-18:00 Beijing time (DeepSeek's peak rate)")
			}
			for _, t := range p.Tiers {
				fmt.Printf("    over %s prompt tokens: %s\n", render.Tokens(t.Above), rates(t.Flat))
			}
		}
		return 0
	}
	fmt.Fprintln(os.Stderr, "git tab prices: update | show <model>...")
	return 2
}

// rates is a price as the bill applies it.
func rates(f price.Flat) string {
	b := f.Billed()
	s := fmt.Sprintf("input $%g · output $%g · cache read $%g · cache write $%g", b.Input, b.Output, b.CacheRead, b.CacheWrite)
	if b.CacheWrite1h != b.CacheWrite {
		s += fmt.Sprintf(" (1h $%g)", b.CacheWrite1h)
	}
	return s
}

func doctorCmd() int {
	a, b := gitx.Version()
	fmt.Printf("git-tab %s · git %d.%d (config hooks: %v) · binary %s\n", Version, a, b, gitx.ConfigHooks(), install.Binary())
	fmt.Println("\nhooks")
	st := install.Status("")
	if len(st) == 0 {
		fmt.Println("  not installed here — `git tab install` (or --global)")
	}
	for _, s := range st {
		fmt.Println("  " + s)
	}
	fmt.Println("\nsession logs")
	for _, ad := range agents.All() {
		for _, root := range ad.Roots() {
			n, newest := 0, time.Time{}
			filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() && isLog(d.Name()) {
					n++
					if info, err := d.Info(); err == nil && info.ModTime().After(newest) {
						newest = info.ModTime()
					}
				}
				return nil
			})
			if n == 0 {
				fmt.Printf("  %-12s %s: none\n", ad.Name(), root)
				continue
			}
			fmt.Printf("  %-12s %s: %d files, newest %s\n", ad.Name(), root, n, newest.Local().Format("2006-01-02 15:04"))
		}
	}
	c := price.Load()
	fmt.Printf("\nprices\n  %s catalog, fetched %s (%s)\n", c.From, c.Fetched.Local().Format("2006-01-02"), price.CachePath())
	if sc := stamp.EnvScope(); sc != nil {
		fmt.Printf("\nthis shell is agent session %s: its commits settle only that session\n", sc)
	}
	repo, err := gitx.Open("")
	if err != nil {
		return 0
	}
	fmt.Printf("\nrepository %s\n", repo.Top)
	if l, err := ledger.Open(repo.CommonDir); err == nil && !l.Fresh() {
		fmt.Printf("  settled up to %s; %d stamped commits remembered\n", l.State.Base.Local().Format("2006-01-02 15:04:05"), len(l.Commits))
		var keys []string
		for k := range l.State.Marks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  session %s settled up to %s\n", k, l.State.Marks[k].Local().Format("2006-01-02 15:04:05"))
		}
	} else {
		fmt.Println("  no ledger yet: the first commit settles what came after HEAD")
	}
	for _, ev := range install.Events {
		if b, err := os.ReadFile(filepath.Join(repo.GitDir, "git-tab", "last-"+ev+".log")); err == nil {
			fmt.Print("\nlast " + strings.TrimRight(string(b), "\n") + "\n")
		}
	}
	return 0
}

// isLog says a file is one of the session logs or databases agents keep.
func isLog(name string) bool {
	for _, suf := range []string{".jsonl", ".jsonl.zst", ".jsonl.zstd", ".db", ".sqlite"} {
		if strings.HasSuffix(name, suf) {
			return true
		}
	}
	return false
}

func count(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
