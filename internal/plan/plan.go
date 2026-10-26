// Package plan is what a person actually pays for agent usage, next to its
// API price: a subscription's monthly fee spread over the usage it covers,
// a reseller's fixed share of API prices, or API prices for a key paid as
// it goes.
//
// Plans live in git config, normally the global one, as
//
//	[tab-plan "claude-max"]
//		for = claude-code, pi/anthropic
//		fee = 125
//		worth = 2000
//	[tab-plan "deepseek"]
//		for = deepseek
//		share = 1
//
// for names what the plan pays for: an agent (claude-code), a provider the
// agent calls (deepseek, opencode-go) or the maker of the model, or both
// joined by a slash. A subscription's share of API prices is its monthly
// fee over what a month of the usage it pays for is worth at those prices:
// the worth its owner states, or else the measured usage of the 30 days
// before at, which `git tab plan` reads from this machine's logs.
package plan

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fish2lab/git-tab/internal/agents"
	"github.com/fish2lab/git-tab/internal/price"
)

// Window is how much usage a subscription's share is measured over.
const Window = 30 * 24 * time.Hour

type Plan struct {
	Name     string
	For      []string
	Fee      float64 // USD a month; 0 for a plan paid per token
	Share    float64 // of API prices
	Worth    float64 // a month of its usage at API prices, as its owner says
	Measured float64 // the same, as this machine's logs say, on At
	At       time.Time
}

// Stale says a subscription's measured worth is due for a new measurement.
func (p Plan) Stale(now time.Time, age time.Duration) bool {
	return p.Fee > 0 && p.Worth <= 0 && now.Sub(p.At) > age
}

// Book is a person's plans, found by what they pay for.
type Book struct {
	Plans []Plan
	by    map[string]int
}

// FromConfig reads the tab-plan.<name>.<key> entries of a git config map
// with lower-cased keys.
func FromConfig(cfg map[string]string) *Book {
	plans := map[string]*Plan{}
	for k, v := range cfg {
		rest, ok := strings.CutPrefix(k, "tab-plan.")
		if !ok {
			continue
		}
		i := strings.LastIndexByte(rest, '.')
		if i <= 0 {
			continue
		}
		name, key := rest[:i], rest[i+1:]
		p := plans[name]
		if p == nil {
			p = &Plan{Name: name}
			plans[name] = p
		}
		f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
		switch key {
		case "for":
			p.For = Selectors(v)
		case "fee":
			p.Fee = f
		case "share":
			p.Share = f
		case "worth":
			p.Worth = f
		case "measured":
			p.Measured = f
		case "at":
			p.At, _ = time.Parse(time.DateOnly, strings.TrimSpace(v))
		}
	}
	b := &Book{by: map[string]int{}}
	names := make([]string, 0, len(plans))
	for n := range plans {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := plans[n]
		if p.Fee > 0 {
			p.Share = 0
			if w := p.Base(); w > 0 {
				p.Share = p.Fee / w
			}
		}
		if len(p.For) == 0 {
			continue
		}
		b.add(*p)
	}
	return b
}

// FromEnv reads plans from GIT_TAB_PLANS, for machines without the
// person's git config (a cloud box): plans joined by ";", each
// name=terms[@worth]:for,for — what `git tab plan export` prints. They
// come as tab-plan.* entries to merge into a config map.
func FromEnv(v string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(v, ";") {
		name, rest, ok := strings.Cut(strings.TrimSpace(p), "=")
		terms, sel, ok2 := strings.Cut(rest, ":")
		if !ok || !ok2 || name == "" {
			continue
		}
		terms, worth, _ := strings.Cut(terms, "@")
		fee, share, err := Terms(terms)
		if err != nil {
			continue
		}
		k := "tab-plan." + name + "."
		m[k+"for"] = sel
		if fee > 0 {
			m[k+"fee"] = strconv.FormatFloat(fee, 'f', -1, 64)
			m[k+"worth"] = worth
		} else {
			m[k+"share"] = strconv.FormatFloat(share, 'f', -1, 64)
		}
	}
	return m
}

// Export is b as GIT_TAB_PLANS: subscriptions carry their worth, so the
// share is the same wherever it is read.
func (b *Book) Export() string {
	var out []string
	for _, p := range b.all() {
		terms := "x" + strconv.FormatFloat(p.Share, 'f', -1, 64)
		if p.Fee > 0 {
			if p.Base() <= 0 {
				continue
			}
			terms = strconv.FormatFloat(p.Fee, 'f', -1, 64) + "/mo@" + strconv.FormatFloat(p.Base(), 'f', -1, 64)
		} else if p.Share == 1 {
			terms = "api"
		}
		out = append(out, p.Name+"="+terms+":"+strings.Join(p.For, ","))
	}
	return strings.Join(out, ";")
}

// Base is the monthly worth a subscription's share divides its fee by.
func (p Plan) Base() float64 {
	if p.Worth > 0 {
		return p.Worth
	}
	return p.Measured
}

// Selectors splits a plan's for list.
func Selectors(v string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(strings.ToLower(v), func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, s)
	}
	return out
}

// Keys are what an event can be paid for under, most specific first. Who
// bills a call (the provider, else the model's maker) comes before the
// agent alone: a plan for claude-code does not pay for the DeepSeek calls
// a gateway routes it to when a deepseek plan does.
func Keys(e agents.Event) []string {
	maker := price.Maker(e.Model)
	route := strings.ToLower(e.Provider)
	var out []string
	add := func(s string) {
		if s != "" && s != "/" && !strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/") {
			for _, o := range out {
				if o == s {
					return
				}
			}
			out = append(out, s)
		}
	}
	add(e.Agent + "/" + route)
	add(e.Agent + "/" + maker)
	add(route)
	add(maker)
	add(e.Agent)
	return out
}

// Of is the plan that pays for an event.
func (b *Book) Of(e agents.Event) (*Plan, bool) {
	if b == nil {
		return nil, false
	}
	for _, k := range Keys(e) {
		if i, ok := b.by[k]; ok {
			return &b.Plans[i], true
		}
	}
	return nil, false
}

// Share is the fraction of an event's API price its plan pays. A
// subscription not yet measured has none.
func (b *Book) Share(e agents.Event) (float64, bool) {
	p, ok := b.Of(e)
	if !ok || p.Share <= 0 {
		return 0, false
	}
	return p.Share, true
}

func (b *Book) Empty() bool { return b == nil || len(b.Plans) == 0 }

// Terms is how a plan is paid, as typed: "125/mo", "$1500/yr", "api"
// (API prices), "x0.25" or "25%" (a share of them).
func Terms(s string) (fee, share float64, err error) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "api" || s == "key" || s == "payg":
		return 0, 1, nil
	case strings.HasSuffix(s, "%"):
		v, err := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if err != nil || v <= 0 {
			return 0, 0, fmt.Errorf("bad share %q", s)
		}
		return 0, v / 100, nil
	case strings.HasPrefix(s, "x"):
		v, err := strconv.ParseFloat(s[1:], 64)
		if err != nil || v <= 0 {
			return 0, 0, fmt.Errorf("bad share %q", s)
		}
		return 0, v, nil
	}
	months := 1.0
	for _, suf := range []struct {
		s string
		m float64
	}{{"/mo", 1}, {"/month", 1}, {"/yr", 12}, {"/year", 12}} {
		if v, ok := strings.CutSuffix(s, suf.s); ok {
			s, months = v, suf.m
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimPrefix(s, "$"), 64)
	if err != nil || v <= 0 {
		return 0, 0, errors.New(`say how the plan is paid: "125/mo", "1500/yr", "api" or "x0.25"`)
	}
	return v / months, 0, nil
}

type Pricer interface {
	Cost(agents.Event) (float64, bool)
}

// Row is one kind of usage: what a plan could pay for (the agent and the
// provider it called, or the model's maker), its API price and the plan
// that pays for it, if one does.
type Row struct {
	Key  string
	Cost float64
	Plan *Plan
}

// Spend is events' API-priced usage by kind, priciest first.
func (b *Book) Spend(events []agents.Event, p Pricer) []Row {
	rows := map[string]*Row{}
	for _, e := range events {
		if e.Kind != agents.Request {
			continue
		}
		c, ok := p.Cost(e)
		if !ok {
			continue
		}
		k := Keys(e)[0]
		r := rows[k]
		if r == nil {
			r = &Row{Key: k}
			r.Plan, _ = b.Of(e)
			rows[k] = r
		}
		r.Cost += c
	}
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Cost > out[j].Cost })
	return out
}

// Measure is a month of the API-priced usage each plan pays for among
// events, which reach from from to now. Logs that reach back less than a
// Window are scaled up to one; days is how many they cover.
func (b *Book) Measure(events []agents.Event, p Pricer, from, now time.Time) (worth map[string]float64, days float64) {
	worth = map[string]float64{}
	first := now
	for _, e := range events {
		if e.Kind != agents.Request || e.Time.Before(from) {
			continue
		}
		if e.Time.Before(first) {
			first = e.Time
		}
		pl, ok := b.Of(e)
		if !ok {
			continue
		}
		if c, ok := p.Cost(e); ok {
			worth[pl.Name] += c
		}
	}
	span := now.Sub(first)
	if span < 24*time.Hour {
		span = 24 * time.Hour
	}
	if span < Window {
		for k := range worth {
			worth[k] *= float64(Window) / float64(span)
		}
	}
	return worth, span.Hours() / 24
}

// With is b with p in it, replacing a plan of the same name; Without is b
// without the plan named.
func (b *Book) With(p Plan) *Book {
	n := &Book{by: map[string]int{}}
	for _, q := range b.all() {
		if q.Name != p.Name {
			n.add(q)
		}
	}
	n.add(p)
	return n
}

func (b *Book) Without(name string) *Book {
	n := &Book{by: map[string]int{}}
	for _, q := range b.all() {
		if q.Name != name {
			n.add(q)
		}
	}
	return n
}

func (b *Book) all() []Plan {
	if b == nil {
		return nil
	}
	return b.Plans
}

func (b *Book) add(p Plan) {
	b.Plans = append(b.Plans, p)
	for _, s := range p.For {
		if _, taken := b.by[s]; !taken {
			b.by[s] = len(b.Plans) - 1
		}
	}
}
