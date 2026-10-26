// Package render writes a bill as commit trailers, in one of the templates,
// and places them in a commit message.
package render

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"github.com/fish2lab/git-tab/internal/bill"
)

const (
	Minimal  = "minimal"
	Standard = "standard"
	Receipt  = "receipt"
	Fun      = "fun"
)

var Templates = []string{Minimal, Standard, Receipt, Fun}

// Schema is the version of the trailer format, written as AI-Cost-Schema.
// The one-line AI-Cost of minimal and fun is schema 1 by its "(api-equiv)"
// tag; a format that changes must write AI-Cost-Schema: 2 or later.
const Schema = 1

// Keys are the trailers this package writes; Strip removes exactly these.
var Keys = []string{"AI-Cost", "AI-Cost-Paid", "AI-Tokens", "AI-Model", "AI-Agent", "AI-Turns", "AI-Session",
	"AI-Cost-Equiv", "AI-Cost-Schema"}

// Options say how to render a bill.
type Options struct {
	Template string
	Snacks   []Snack
	Tab      *Tab // the commits before this one, for the receipt's running tab
}

// Tab is what the receipts already in a branch's history add up to.
type Tab struct {
	Commits int
	Cost    float64
	Paid    float64
	Planned bool // some of them carry a paid amount
}

// Snack is a thing a dollar amount can be counted in.
type Snack struct {
	Name string
	USD  float64
}

// DefaultSnacks are what a receipt counts money in unless tab.snacks says
// otherwise; ChineseSnacks are what `git tab install` sets in China.
const (
	DefaultSnacks = "coffees=4.5,pizza slices=3.5"
	ChineseSnacks = "杯咖啡=4.5,个煎饼果子=1.4"
)

// ParseSnacks reads "name=usd,name=usd".
func ParseSnacks(s string) []Snack {
	var out []Snack
	for _, part := range strings.Split(s, ",") {
		name, v, ok := strings.Cut(part, "=")
		usd, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if ok && err == nil && usd > 0 && strings.TrimSpace(name) != "" {
			out = append(out, Snack{strings.TrimSpace(name), usd})
		}
	}
	return out
}

// USD formats a dollar amount with the precision a receipt needs.
func USD(c float64) string {
	switch {
	case c >= 1000:
		s := fmt.Sprintf("%.2f", c)
		i := strings.IndexByte(s, '.')
		for j := i - 3; j > 0; j -= 3 {
			s = s[:j] + "," + s[j:]
		}
		return "$" + s
	case c >= 1 || c == 0:
		return fmt.Sprintf("$%.2f", c)
	case c >= 0.01:
		return fmt.Sprintf("$%.3f", c)
	default:
		return strings.TrimSuffix(fmt.Sprintf("$%.4f", c), "0")
	}
}

// Tokens formats a token count: 812, 9.1k, 412k, 1.2M, 37M.
func Tokens(n int64) string {
	f := float64(n)
	unit := ""
	switch {
	case n >= 1e6:
		f, unit = f/1e6, "M"
	case n >= 1e3:
		f, unit = f/1e3, "k"
	default:
		return strconv.FormatInt(n, 10)
	}
	if f < 10 {
		return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0") + unit
	}
	return fmt.Sprintf("%.0f", f) + unit
}

// Duration formats a span of busy time: 45s, 23m, 1h05m.
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

// Equiv is a dollar amount counted in snacks: "≈ 0.4 杯咖啡 / 1.3 个煎饼果子".
func Equiv(c float64, snacks []Snack) string {
	var parts []string
	for _, s := range snacks {
		n := c / s.USD
		v := fmt.Sprintf("%.0f", n)
		switch {
		case n < 0.1:
			v = fmt.Sprintf("%.2f", n)
		case n < 10:
			v = strings.TrimSuffix(fmt.Sprintf("%.1f", n), ".0")
		}
		parts = append(parts, v+" "+s.Name)
	}
	if len(parts) == 0 {
		return ""
	}
	return "≈ " + strings.Join(parts, " / ")
}

func costValue(b *bill.Bill) string {
	if len(b.Unpriced) == len(b.Lines) {
		return "unpriced (api-equiv)"
	}
	if len(b.Unpriced) > 0 {
		return USD(b.Cost) + " (api-equiv; unpriced: " + strings.Join(b.Unpriced, ", ") + ")"
	}
	return USD(b.Cost) + " (api-equiv)"
}

// paidValue is AI-Cost-Paid: the plan-adjusted amount and its share of
// the API price.
func paidValue(b *bill.Bill) string {
	v := USD(b.Paid)
	if b.Cost > 0 {
		v += fmt.Sprintf(" (%s of api-equiv)", percent(b.Paid/b.Cost))
	}
	return v
}

// percent formats a share: 94%, 6.25%, 0.4%.
func percent(f float64) string {
	p := f * 100
	if p >= 10 || p == 0 {
		return fmt.Sprintf("%.0f%%", p)
	}
	return strconv.FormatFloat(p, 'g', 3, 64) + "%"
}

func oneLine(b *bill.Bill) string {
	parts := []string{costValue(b)}
	if b.Planned {
		parts = append(parts, "paid "+USD(b.Paid))
	}
	if len(b.Lines) > 0 {
		m := b.Lines[0].Model
		if len(b.Lines) > 1 {
			m += fmt.Sprintf(" +%d", len(b.Lines)-1)
		}
		parts = append(parts, m)
	}
	if b.Turns > 0 {
		parts = append(parts, plural(b.Turns, "turn"))
	}
	return "AI-Cost: " + strings.Join(parts, " · ")
}

func tokenLine(b *bill.Bill) string {
	u := b.Usage
	s := "in=" + Tokens(u.Prompt())
	if u.CacheRead > 0 {
		s += " cached=" + Tokens(u.CacheRead)
	}
	if u.Written() > 0 {
		s += " written=" + Tokens(u.Written())
	}
	return s + " out=" + Tokens(u.Output)
}

func standard(b *bill.Bill) []string {
	models := make([]string, 0, len(b.Lines))
	for _, l := range b.Lines {
		switch {
		case !l.Priced:
			models = append(models, l.Model+" (unpriced)")
		case len(b.Lines) > 1:
			models = append(models, l.Model+" "+USD(l.Cost))
		default:
			models = append(models, l.Model)
		}
	}
	out := []string{"AI-Cost: " + costValue(b)}
	if b.Planned {
		out = append(out, "AI-Cost-Paid: "+paidValue(b))
	}
	out = append(out,
		"AI-Tokens: "+tokenLine(b),
		"AI-Model: "+strings.Join(models, ", "),
		"AI-Agent: "+strings.Join(b.Agents, ", "),
	)
	if b.Turns > 0 {
		out = append(out, fmt.Sprintf("AI-Turns: %d", b.Turns))
	}
	if len(b.SessionIDs) > 0 {
		out = append(out, "AI-Session: "+strings.Join(b.SessionIDs, ", "))
	}
	return append(out, fmt.Sprintf("AI-Cost-Schema: %d", Schema))
}

// Render is a bill as body art (receipt only) and trailer lines.
func Render(b *bill.Bill, o Options) (art, trailers []string) {
	switch o.Template {
	case Minimal:
		return nil, []string{oneLine(b)}
	case Fun:
		out := []string{oneLine(b)}
		if e := Equiv(b.Cost, o.Snacks); e != "" && len(b.Unpriced) < len(b.Lines) {
			out = append(out, "AI-Cost-Equiv: "+e)
		}
		return nil, out
	case Standard:
		return nil, standard(b)
	default:
		return receiptArt(b, o), standard(b)
	}
}

// ReceiptMark is in the header of every receipt Render draws, so Strip can
// find one again.
const ReceiptMark = "A I   R E C E I P T"

const boxWidth = 36

// width is how many terminal columns s takes, East Asian wide characters
// and emoji being two.
func width(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
			r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
			r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1FAFF, r >= 0x20000 && r <= 0x3FFFD:
			n += 2
		default:
			n++
		}
	}
	return n
}

func clip(s string, w int) string {
	for width(s) > w {
		r := []rune(s)
		s = string(r[:len(r)-1])
	}
	return s
}

// Box draws lines as a paper receipt torn off at the bottom.
func Box(lines []string) []string {
	out := []string{" " + strings.Repeat("_", boxWidth)}
	for _, l := range lines {
		l = clip(l, boxWidth-1)
		out = append(out, "| "+l+strings.Repeat(" ", boxWidth-1-width(l))+"|")
	}
	out = append(out, "|"+strings.Repeat("_", boxWidth)+"|")
	return append(out, " "+strings.Repeat(`\/`, boxWidth/2))
}

// Center pads s to the middle of a receipt line.
func Center(s string) string {
	pad := (boxWidth - 1 - width(s)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + s
}

// Item is a receipt line with a price flush right.
func Item(name, cost string) string {
	room := boxWidth - 2 - width(cost)
	name = clip(name, room-1)
	return name + strings.Repeat(" ", room-width(name)) + cost
}

// Barcode is a decorative bar pattern drawn from seed.
func Barcode(seed string) string {
	h := fnv.New64a()
	h.Write([]byte(seed))
	v := h.Sum64()
	var sb strings.Builder
	for i := 0; i < 28; i++ {
		if v&1 == 1 {
			sb.WriteString("|")
		} else {
			sb.WriteString(" ")
		}
		if v&2 == 2 {
			sb.WriteString("|")
		}
		v = v>>2 | v<<62
	}
	return clip(sb.String(), boxWidth-5)
}

func receiptArt(b *bill.Bill, o Options) []string {
	l := []string{
		"",
		Center(ReceiptMark),
		Center("git-tab · USD at API prices"),
		Center(b.End.Local().Format("2006-01-02 15:04 MST")),
		"",
	}
	for _, line := range b.Lines {
		cost := USD(line.Cost)
		if !line.Priced {
			cost = "n/a"
		}
		u := line.Usage
		l = append(l, Item(line.Model, cost))
		detail := "  in " + Tokens(u.Prompt())
		if u.CacheRead > 0 {
			detail += "  cached " + Tokens(u.CacheRead)
		}
		l = append(l, detail)
		detail = "  out " + Tokens(u.Output)
		if u.Written() > 0 {
			detail += "  written " + Tokens(u.Written())
		}
		l = append(l, detail+"  x"+strconv.Itoa(line.Requests))
	}
	rule := strings.Repeat("- ", (boxWidth-2)/2)
	l = append(l, rule)
	if b.Planned {
		l = append(l, Item("SUBTOTAL", USD(b.Cost)))
		if d := b.Paid - b.Cost; d <= 0 {
			l = append(l, Item("PLAN DISCOUNT "+percent(-d/b.Cost), "-"+USD(-d)))
		} else {
			l = append(l, Item("PLAN SURCHARGE "+percent(d/b.Cost), "+"+USD(d)))
		}
		l = append(l, Item("YOU PAID", USD(b.Paid)))
	} else {
		l = append(l, Item("TOTAL", USD(b.Cost)))
	}
	stats := []string{plural(b.Requests, "call")}
	if b.Turns > 0 {
		stats = append([]string{plural(b.Turns, "turn")}, stats...)
	}
	if b.Busy >= time.Second {
		stats = append(stats, Duration(b.Busy)+" busy")
	}
	l = append(l, strings.Join(stats, " · "))
	if e := Equiv(b.Cost, o.Snacks); e != "" {
		l = append(l, e)
	}
	if t := o.Tab; t != nil && t.Commits > 0 {
		l = append(l, rule, Item("TAB SO FAR · "+plural(t.Commits+1, "commit"), USD(t.Cost+b.Cost)))
		if t.Planned || b.Planned {
			paid := t.Paid + b.Cost
			if b.Planned {
				paid = t.Paid + b.Paid
			}
			l = append(l, Item("  paid", USD(paid)))
		}
	}
	l = append(l, "", "  "+Barcode(b.End.String()+USD(b.Cost)), Center("THANK YOU, COME AGAIN"))
	return Box(l)
}
