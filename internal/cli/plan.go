package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/fish2lab/git-tab/internal/agents"
	"github.com/fish2lab/git-tab/internal/gitx"
	"github.com/fish2lab/git-tab/internal/plan"
	"github.com/fish2lab/git-tab/internal/price"
	"github.com/fish2lab/git-tab/internal/render"
)

const planUsage = `git tab plan                                    your plans, and a month of usage by what pays for it
git tab plan add <name> <terms> <for>... [--worth USD]
                                                terms: 125/mo, 1500/yr (a subscription), api (a key at
                                                list price), x0.5 or 50% (a share of list price)
                                                for: an agent, a provider or both, e.g. claude-code,
                                                deepseek, pi/anthropic, opencode/opencode-go
git tab plan rm <name>
git tab plan measure                            re-measure what each subscription's month is worth
`

// plans are the tab-plan.* entries of the global git config: what a
// person pays is the same in every repository.
func plans() *plan.Book {
	out, _ := gitx.Run("", "config", "--global", "--get-regexp", `^tab-plan\.`)
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, " "); ok {
			m[strings.ToLower(k)] = v
		}
	}
	return plan.FromConfig(m)
}

func setPlan(name, key, value string) error {
	_, err := gitx.Run("", "config", "--global", "tab-plan."+name+"."+key, value)
	return err
}

func planCmd(args []string) int {
	if len(args) == 0 {
		return planList()
	}
	switch args[0] {
	case "add", "set":
		return planAdd(args[1:])
	case "rm", "remove":
		if len(args) != 2 {
			break
		}
		if _, err := gitx.Run("", "config", "--global", "--remove-section", "tab-plan."+args[1]); err != nil {
			fmt.Fprintf(os.Stderr, "git tab: no plan %q\n", args[1])
			return 1
		}
		fmt.Printf("✓ removed plan %s; its usage is priced at API rates again\n", args[1])
		return 0
	case "measure":
		return planMeasure(len(args) > 1 && args[1] == "--quiet")
	case "help", "-h", "--help":
		fmt.Print(planUsage)
		return 0
	}
	fmt.Fprint(os.Stderr, planUsage)
	return 2
}

func planAdd(args []string) int {
	fs := newFlags("plan add")
	worth := fs.Float64("worth", 0, "what a month of the plan's usage is worth at API prices, if you know better than this machine's logs")
	// Flags may come after the positional arguments.
	var pos []string
	for len(args) > 0 {
		if fs.Parse(args) != nil {
			return 2
		}
		if fs.NArg() == 0 {
			break
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
	if len(pos) < 3 {
		fmt.Fprint(os.Stderr, planUsage)
		return 2
	}
	name, terms, sel := pos[0], pos[1], plan.Selectors(strings.Join(pos[2:], ","))
	fee, share, err := plan.Terms(terms)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git tab:", err)
		return 2
	}
	gitx.Run("", "config", "--global", "--remove-section", "tab-plan."+name)
	steps := [][2]string{{"for", strings.Join(sel, ", ")}}
	if fee > 0 {
		steps = append(steps, [2]string{"fee", strconv.FormatFloat(fee, 'f', -1, 64)})
		if *worth > 0 {
			steps = append(steps, [2]string{"worth", strconv.FormatFloat(*worth, 'f', -1, 64)})
		}
	} else {
		steps = append(steps, [2]string{"share", strconv.FormatFloat(share, 'f', -1, 64)})
	}
	for _, s := range steps {
		if err := setPlan(name, s[0], s[1]); err != nil {
			fmt.Fprintln(os.Stderr, "git tab:", err)
			return 1
		}
	}
	if fee > 0 && *worth <= 0 {
		if planMeasure(false) != 0 {
			return 1
		}
	}
	return planList()
}

// month is the last Window of usage in every log on this machine.
func month(now time.Time) []agents.Event {
	var events []agents.Event
	agents.Scan(agents.Query{Since: now.Add(-plan.Window), Until: now}, func(e agents.Event) {
		if e.Kind == agents.Request {
			events = append(events, e)
		}
	})
	return events
}

func planMeasure(quiet bool) int {
	b := plans()
	now := time.Now()
	var due []plan.Plan
	for _, p := range b.Plans {
		if p.Fee > 0 && p.Worth <= 0 {
			due = append(due, p)
		}
	}
	if len(due) == 0 {
		if !quiet {
			fmt.Println("no subscription to measure: each one has a stated --worth, or there is none")
		}
		return 0
	}
	worth, days := b.Measure(month(now), price.Load(), now.Add(-plan.Window), now)
	for _, p := range due {
		setPlan(p.Name, "measured", strconv.FormatFloat(round2(worth[p.Name]), 'f', -1, 64))
		setPlan(p.Name, "at", now.Format(time.DateOnly))
		if quiet {
			continue
		}
		note := ""
		if days < 29 {
			note = fmt.Sprintf(" (the logs cover %.0f days; scaled to 30)", days)
		}
		fmt.Printf("✓ %s: a month of its usage is worth %s at API prices%s\n", p.Name, render.USD(worth[p.Name]), note)
	}
	return 0
}

func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

func planList() int {
	b := plans()
	now := time.Now()
	fmt.Println("plans (git config --global tab-plan.*)")
	if len(b.Plans) == 0 {
		fmt.Println("  none: every receipt shows API prices only")
	}
	for _, p := range b.Plans {
		terms := "api key at list price"
		switch {
		case p.Fee > 0:
			terms = render.USD(p.Fee) + "/mo"
			switch {
			case p.Worth > 0:
				terms += fmt.Sprintf(", worth %s/mo at API prices (as you said)", render.USD(p.Worth))
			case p.Measured > 0:
				terms += fmt.Sprintf(", worth %s/mo at API prices (measured %s)", render.USD(p.Measured), p.At.Format(time.DateOnly))
			default:
				terms += ", not measured yet: `git tab plan measure`"
			}
		case p.Share != 1:
			terms = fmt.Sprintf("%g× list price", p.Share)
		}
		pays := "-"
		if p.Share > 0 {
			pays = fmt.Sprintf("pays %.4g%% of API prices", p.Share*100)
		}
		fmt.Printf("  %-14s for %-24s %s · %s\n", p.Name, strings.Join(p.For, ", "), terms, pays)
	}
	rows := b.Spend(month(now), price.Load())
	fmt.Println("\nthe last 30 days on this machine, at API prices")
	if len(rows) == 0 {
		fmt.Println("  no agent usage")
	}
	var open []string
	for _, r := range rows {
		paid := "no plan: API prices"
		if r.Plan != nil && r.Plan.Share > 0 {
			paid = fmt.Sprintf("%s · you paid %s", r.Plan.Name, render.USD(r.Cost*r.Plan.Share))
		} else if r.Plan != nil {
			paid = r.Plan.Name + " · not measured yet"
		} else if r.Cost >= 0.01 {
			open = append(open, r.Key)
		}
		fmt.Printf("  %-34s %11s  %s\n", r.Key, render.USD(r.Cost), paid)
	}
	if len(open) > 0 {
		fmt.Printf("\nif a subscription or a discounted key pays for %s:\n", strings.Join(open, ", "))
		fmt.Println("  git tab plan add <name> 125/mo <for>        a subscription (add --worth 2000 if you know its monthly worth)")
		fmt.Println("  git tab plan add <name> api <for>           a key at list price")
		fmt.Println("  git tab plan add <name> x0.5 <for>          a key at half price")
	}
	return 0
}
