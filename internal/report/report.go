// Package report reads the receipts back out of a repository's history:
// `git tab summary`, `git tab receipt` and `git tab badge`.
package report

import (
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fish2lab/git-tab/internal/gitx"
	"github.com/fish2lab/git-tab/internal/render"
)

// Entry is one commit's receipt.
type Entry struct {
	SHA     string
	Time    time.Time
	Subject string
	Cost    float64
	Paid    float64 // what a plan made it cost, or Cost without one
	Planned bool
	Priced  bool
	Models  []Share
	Agents  []string
	In      int64
	Cached  int64
	Written int64
	Out     int64
	Turns   int
}

// Share is a model's part of a commit's cost.
type Share struct {
	Model string
	Cost  float64
}

type History struct {
	Repo    string
	Commits int // commits read, with a receipt or not
	Entries []Entry
}

var (
	costRe   = regexp.MustCompile(`^\$(\d+(?:\.\d+)?)`)
	tokenRe  = regexp.MustCompile(`(in|cached|written|out)=([\d.]+)([kM]?)`)
	modelRe  = regexp.MustCompile(`^(.+?)(?: \$(\d+(?:\.\d+)?)| \(unpriced\))?$`)
	turnsRe  = regexp.MustCompile(`^(\d+) turns?$`)
	extraRe  = regexp.MustCompile(` \+\d+$`)
	fieldSep = "\x1f"
)

// Load reads the receipts of the commits revs name (HEAD's history when
// none are given).
func Load(dir string, revs []string) (*History, error) {
	format := "--format=%H%x1f%ct%x1f%s%x1f%(trailers:key=AI-Cost,key=AI-Cost-Paid,key=AI-Tokens,key=AI-Model,key=AI-Agent,key=AI-Turns,unfold,separator=%x1d)%x1e"
	args := append([]string{"log", format}, revs...)
	if len(revs) == 0 {
		args = append(args, "HEAD")
	}
	out, err := gitx.Run(dir, args...)
	if err != nil {
		return nil, err
	}
	top, _ := gitx.Run(dir, "rev-parse", "--show-toplevel")
	h := &History{Repo: filepath.Base(top)}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		f := strings.Split(rec, fieldSep)
		if len(f) < 4 {
			continue
		}
		h.Commits++
		if e, ok := parse(f); ok {
			h.Entries = append(h.Entries, e)
		}
	}
	return h, nil
}

func parse(f []string) (Entry, bool) {
	sec, _ := strconv.ParseInt(f[1], 10, 64)
	e := Entry{SHA: f[0], Time: time.Unix(sec, 0), Subject: f[2]}
	found := false
	for _, t := range strings.Split(f[3], "\x1d") {
		k, v, ok := strings.Cut(strings.TrimSpace(t), ": ")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "AI-Cost":
			if !strings.Contains(v, "(api-equiv") {
				continue // someone else's AI-Cost
			}
			found = true
			parts := strings.Split(v, " · ")
			if m := costRe.FindStringSubmatch(parts[0]); m != nil {
				e.Cost, _ = strconv.ParseFloat(m[1], 64)
				e.Priced = true
			}
			for _, p := range parts[1:] {
				if paid, ok := strings.CutPrefix(p, "paid "); ok {
					if m := costRe.FindStringSubmatch(paid); m != nil {
						e.Paid, _ = strconv.ParseFloat(m[1], 64)
						e.Planned = true
					}
				} else if m := turnsRe.FindStringSubmatch(p); m != nil {
					e.Turns, _ = strconv.Atoi(m[1])
				} else if len(e.Models) == 0 {
					e.Models = []Share{{Model: extraRe.ReplaceAllString(p, ""), Cost: math.NaN()}}
				}
			}
		case "AI-Cost-Paid":
			if m := costRe.FindStringSubmatch(v); m != nil {
				e.Paid, _ = strconv.ParseFloat(m[1], 64)
				e.Planned = true
			}
		case "AI-Tokens":
			for _, m := range tokenRe.FindAllStringSubmatch(v, -1) {
				n := tokens(m[2], m[3])
				switch m[1] {
				case "in":
					e.In = n
				case "cached":
					e.Cached = n
				case "written":
					e.Written = n
				case "out":
					e.Out = n
				}
			}
		case "AI-Model":
			e.Models = nil
			for _, part := range strings.Split(v, ", ") {
				if m := modelRe.FindStringSubmatch(part); m != nil {
					s := Share{Model: m[1], Cost: math.NaN()}
					if m[2] != "" {
						s.Cost, _ = strconv.ParseFloat(m[2], 64)
					} else if strings.HasSuffix(part, "(unpriced)") {
						s.Cost = 0
					}
					e.Models = append(e.Models, s)
				}
			}
		case "AI-Agent":
			for _, a := range strings.Split(v, ", ") {
				e.Agents = append(e.Agents, strings.TrimSpace(a))
			}
		case "AI-Turns":
			e.Turns, _ = strconv.Atoi(v)
		}
	}
	if !e.Planned {
		e.Paid = e.Cost
	}
	if found && len(e.Agents) == 0 {
		e.Agents = []string{"(not recorded)"}
	}
	// A model listed without its own cost is the commit's whole cost.
	for i := range e.Models {
		if math.IsNaN(e.Models[i].Cost) {
			e.Models[i].Cost = 0
			if len(e.Models) == 1 {
				e.Models[i].Cost = e.Cost
			}
		}
	}
	return e, found
}

func tokens(num, unit string) int64 {
	f, _ := strconv.ParseFloat(num, 64)
	switch unit {
	case "k":
		f *= 1e3
	case "M":
		f *= 1e6
	}
	return int64(f)
}

// Totals are a history's sums.
type Totals struct {
	Cost                     float64
	Paid                     float64
	Planned                  bool // some receipt carries a plan's price
	In, Cached, Written, Out int64
	Turns                    int
	ByModel, ByAgent, ByMon  []Share
	First, Last              time.Time
	Priciest                 []Entry
}

func (h *History) Totals() Totals {
	var t Totals
	models, agents, months := map[string]float64{}, map[string]float64{}, map[string]float64{}
	for _, e := range h.Entries {
		t.Cost += e.Cost
		t.Paid += e.Paid
		t.Planned = t.Planned || e.Planned
		t.In += e.In
		t.Cached += e.Cached
		t.Written += e.Written
		t.Out += e.Out
		t.Turns += e.Turns
		for _, m := range e.Models {
			models[m.Model] += m.Cost
		}
		for _, a := range e.Agents {
			agents[a] += e.Cost / float64(len(e.Agents))
		}
		months[e.Time.Local().Format("2006-01")] += e.Cost
		if t.First.IsZero() || e.Time.Before(t.First) {
			t.First = e.Time
		}
		if e.Time.After(t.Last) {
			t.Last = e.Time
		}
	}
	t.ByModel, t.ByAgent, t.ByMon = shares(models, true), shares(agents, true), shares(months, false)
	t.Priciest = append([]Entry{}, h.Entries...)
	sort.SliceStable(t.Priciest, func(i, j int) bool { return t.Priciest[i].Cost > t.Priciest[j].Cost })
	if len(t.Priciest) > 5 {
		t.Priciest = t.Priciest[:5]
	}
	return t
}

func shares(m map[string]float64, byCost bool) []Share {
	var out []Share
	for k, v := range m {
		out = append(out, Share{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if byCost && out[i].Cost != out[j].Cost {
			return out[i].Cost > out[j].Cost
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func bar(frac float64, width int) string {
	n := int(math.Round(frac * float64(width)))
	if n == 0 && frac > 0 {
		n = 1
	}
	return strings.Repeat("█", n)
}

func pct(part, whole float64) string {
	if whole <= 0 {
		return "  -"
	}
	return fmt.Sprintf("%3.0f%%", 100*part/whole)
}

// Summary is `git tab summary`.
func Summary(h *History) string {
	var b strings.Builder
	t := h.Totals()
	fmt.Fprintf(&b, "git tab summary · %s\n", h.Repo)
	if len(h.Entries) == 0 {
		fmt.Fprintf(&b, "  none of %d commits carries a receipt yet\n", h.Commits)
		return b.String()
	}
	fmt.Fprintf(&b, "  %d of %d commits carry a receipt · %s → %s\n\n", len(h.Entries), h.Commits,
		t.First.Local().Format("2006-01-02"), t.Last.Local().Format("2006-01-02"))
	fmt.Fprintf(&b, "  %-18s %10s  (USD at API prices)\n", "total", render.USD(t.Cost))
	if t.Planned {
		fmt.Fprintf(&b, "  %-18s %10s  (what your plans made it cost)\n", "you paid", render.USD(t.Paid))
	}
	if t.In+t.Out > 0 { // minimal receipts carry no token counts
		fmt.Fprintf(&b, "  %-18s in %s · cached %s · written %s · out %s\n", "tokens",
			render.Tokens(t.In), render.Tokens(t.Cached), render.Tokens(t.Written), render.Tokens(t.Out))
	}
	if t.Turns > 0 {
		fmt.Fprintf(&b, "  %-18s %d\n", "turns", t.Turns)
	}
	fmt.Fprintf(&b, "  %-18s %s per commit\n", "average", render.USD(t.Cost/float64(len(h.Entries))))
	section := func(title string, rows []Share, bars bool) {
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n  %s\n", title)
		for _, r := range rows {
			line := fmt.Sprintf("    %-24s %10s  %s", r.Model, render.USD(r.Cost), pct(r.Cost, t.Cost))
			if bars {
				line += "  " + bar(r.Cost/math.Max(t.Cost, 1e-9), 24)
			}
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	section("by model", t.ByModel, true)
	section("by agent", t.ByAgent, true)
	if len(t.ByMon) > 1 {
		section("by month", t.ByMon, false)
	}
	fmt.Fprintf(&b, "\n  priciest commits\n")
	for _, e := range t.Priciest {
		fmt.Fprintf(&b, "    %10s  %.9s  %s  %s\n", render.USD(e.Cost), e.SHA, e.Time.Local().Format("2006-01-02"), clip(e.Subject, 50))
	}
	return b.String()
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// Receipt is `git tab receipt`: the project's whole bill as a paper receipt.
func Receipt(h *History, snacks []render.Snack) string {
	t := h.Totals()
	l := []string{
		"",
		render.Center(render.ReceiptMark),
		render.Center("project: " + h.Repo),
		render.Center(t.First.Local().Format("2006-01-02") + " → " + t.Last.Local().Format("2006-01-02")),
		"",
	}
	for _, m := range t.ByModel {
		l = append(l, render.Item(m.Model, render.USD(m.Cost)))
	}
	if len(t.ByModel) == 0 {
		l = append(l, render.Center("(nothing on the tab yet)"))
	}
	l = append(l, strings.Repeat("- ", 17), render.Item("TOTAL", render.USD(t.Cost)))
	if t.Planned {
		l = append(l, render.Item("YOU PAID", render.USD(t.Paid)))
	}
	counts := plural(len(h.Entries), "commit")
	if t.Turns > 0 {
		counts += " · " + plural(t.Turns, "turn")
	}
	l = append(l, counts)
	if t.In+t.Out > 0 {
		l = append(l, "in "+render.Tokens(t.In)+" · out "+render.Tokens(t.Out))
	}
	if len(t.Priciest) > 0 {
		p := t.Priciest[0]
		l = append(l, "priciest: "+fmt.Sprintf("%.7s", p.SHA)+" "+render.USD(p.Cost))
	}
	if e := render.Equiv(t.Cost, snacks); e != "" {
		l = append(l, e)
	}
	seed := h.Repo
	if len(h.Entries) > 0 {
		seed += h.Entries[0].SHA
	}
	l = append(l, "", "  "+render.Barcode(seed), render.Center("KEEP FOR YOUR RECORDS"))
	return strings.Join(render.Box(l), "\n") + "\n"
}

// BadgeText is the amount on the badge: cents below $10, whole dollars above.
func BadgeText(cost float64) string {
	if cost < 10 {
		return fmt.Sprintf("$%.2f", cost)
	}
	return fmt.Sprintf("$%.0f", cost)
}

// BadgeURL is a shields.io badge saying what the project cost.
func BadgeURL(cost float64) string {
	msg := strings.ReplaceAll("built with "+BadgeText(cost)+" of AI", " ", "_")
	msg = strings.ReplaceAll(strings.ReplaceAll(msg, "-", "--"), "$", "%24")
	return "https://img.shields.io/badge/" + msg + "-8A2BE2"
}

// BadgeSVG is the same badge drawn locally, for a README that should not
// depend on a service.
func BadgeSVG(cost float64) string {
	label, value := "built with", BadgeText(cost)+" of AI"
	lw, vw := 8+7*len(label), 8+7*len(value)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%[1]d" height="20" role="img" aria-label="%[3]s: %[4]s">
<linearGradient id="s" x2="0" y2="100%%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/><stop offset="1" stop-opacity=".1"/></linearGradient>
<clipPath id="r"><rect width="%[1]d" height="20" rx="3" fill="#fff"/></clipPath>
<g clip-path="url(#r)"><rect width="%[2]d" height="20" fill="#555"/><rect x="%[2]d" width="%[5]d" height="20" fill="#8A2BE2"/><rect width="%[1]d" height="20" fill="url(#s)"/></g>
<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
<text x="%[6]d" y="14">%[3]s</text><text x="%[7]d" y="14">%[4]s</text></g>
</svg>
`, lw+vw, lw, label, value, vw, lw/2, lw+vw/2)
}
