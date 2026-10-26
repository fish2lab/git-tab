// Package bill sums a commit's usage events into the figures its receipt
// shows.
package bill

import (
	"sort"
	"time"

	"github.com/fish2lab/git-tab/internal/agents"
)

// Line is one model's share of a bill.
type Line struct {
	Model    string
	Agent    string
	Usage    agents.Usage
	Requests int
	Cost     float64
	Priced   bool
}

type Bill struct {
	Start, End time.Time // first event, end of the window
	Lines      []Line    // one per model, most expensive first
	Agents     []string  // most expensive first
	Sessions   int
	Turns      int // messages a person typed
	Requests   int // model calls
	Busy       time.Duration
	Usage      agents.Usage
	Cost       float64 // of the priced lines
	Unpriced   []string
}

func (b *Bill) Empty() bool { return b == nil || b.Requests == 0 }

// Pricer prices one request.
type Pricer interface {
	Cost(e agents.Event) (float64, bool)
}

// idle is the longest gap between two events of a thread that still counts
// as the agent being busy.
const idle = 5 * time.Minute

// Build sums events, which must already be the commit's.
func Build(events []agents.Event, p Pricer, end time.Time) *Bill {
	b := &Bill{End: end}
	lines := map[string]*Line{}
	agentCost := map[string]float64{}
	sessions := map[string]bool{}
	threads := map[string][]time.Time{}
	for _, e := range events {
		if b.Start.IsZero() || e.Time.Before(b.Start) {
			b.Start = e.Time
		}
		sessions[e.Agent+":"+e.Session] = true
		threads[e.Agent+":"+e.Thread] = append(threads[e.Agent+":"+e.Thread], e.Time)
		if e.Kind == agents.Prompt {
			b.Turns++
			continue
		}
		b.Requests++
		b.Usage.Add(e.Usage)
		l := lines[e.Model]
		if l == nil {
			l = &Line{Model: e.Model, Agent: e.Agent, Priced: true}
			lines[e.Model] = l
		}
		l.Usage.Add(e.Usage)
		l.Requests++
		if c, ok := p.Cost(e); ok {
			l.Cost += c
			agentCost[e.Agent] += c
		} else {
			l.Priced = false
			if _, ok := agentCost[e.Agent]; !ok {
				agentCost[e.Agent] = 0
			}
		}
	}
	for _, l := range lines {
		if l.Priced {
			b.Cost += l.Cost
		} else {
			b.Unpriced = append(b.Unpriced, l.Model)
		}
		b.Lines = append(b.Lines, *l)
	}
	sort.Slice(b.Lines, func(i, j int) bool {
		if b.Lines[i].Cost != b.Lines[j].Cost {
			return b.Lines[i].Cost > b.Lines[j].Cost
		}
		return b.Lines[i].Usage.Prompt() > b.Lines[j].Usage.Prompt()
	})
	sort.Strings(b.Unpriced)
	for a := range agentCost {
		b.Agents = append(b.Agents, a)
	}
	sort.Slice(b.Agents, func(i, j int) bool {
		if agentCost[b.Agents[i]] != agentCost[b.Agents[j]] {
			return agentCost[b.Agents[i]] > agentCost[b.Agents[j]]
		}
		return b.Agents[i] < b.Agents[j]
	})
	b.Sessions = len(sessions)
	for _, ts := range threads {
		sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
		for i := 1; i < len(ts); i++ {
			b.Busy += min(ts[i].Sub(ts[i-1]), idle)
		}
	}
	return b
}
