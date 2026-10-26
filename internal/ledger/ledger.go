// Package ledger remembers, per repository and on this machine only, which
// agent usage has already been put on a commit's receipt.
package ledger

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// State is what has been settled: everything up to Base, and for a thread
// with a mark (an agent's commit settled it alone), everything up to its mark.
type State struct {
	Base  time.Time            `json:"base"`
	Marks map[string]time.Time `json:"marks,omitempty"`
}

// Start is where a thread's unsettled usage begins.
func (s State) Start(thread string) time.Time {
	if t, ok := s.Marks[thread]; ok {
		return t
	}
	return s.Base
}

// Earliest is the oldest point any thread's unsettled usage can begin.
func (s State) Earliest() time.Time {
	t := s.Base
	for _, m := range s.Marks {
		if m.Before(t) {
			t = m
		}
	}
	return t
}

// Settle is s after a commit settled usage up to end: every thread's, or
// with threads given, only theirs.
func (s State) Settle(end time.Time, threads []string) State {
	n := State{Base: s.Base, Marks: map[string]time.Time{}}
	if len(threads) == 0 {
		if end.After(n.Base) {
			n.Base = end
		}
		for k, t := range s.Marks {
			if t.After(n.Base) {
				n.Marks[k] = t
			}
		}
		return n
	}
	for k, t := range s.Marks {
		n.Marks[k] = t
	}
	for _, k := range threads {
		if end.After(n.Start(k)) {
			n.Marks[k] = end
		}
	}
	return n
}

// Prune is s without the marks older than floor: usage that old never
// counts, so such a mark settles nothing.
func (s State) Prune(floor time.Time) State {
	n := State{Base: s.Base, Marks: map[string]time.Time{}}
	for k, t := range s.Marks {
		if !t.Before(floor) {
			n.Marks[k] = t
		}
	}
	return n
}

// Commit is what a stamped commit settled.
type Commit struct {
	Before  State     `json:"before"` // the state its window was cut from
	End     time.Time `json:"end"`
	Threads []string  `json:"threads,omitempty"` // empty: every thread
	Cost    float64   `json:"cost"`
}

// Tab is what the receipts in a commit's history add up to, kept so that
// the next receipt's running tab costs no walk of the history.
type Tab struct {
	Head    string  `json:"head"`
	Commits int     `json:"commits"` // with a receipt
	Cost    float64 `json:"cost"`
	Paid    float64 `json:"paid"`
	Planned bool    `json:"planned,omitempty"` // some receipt carries a plan's price
}

// Add is t with one more receipt on it, made by commit head.
func (t *Tab) Add(head string, cost, paid float64, planned bool) *Tab {
	return &Tab{Head: head, Commits: t.Commits + 1, Cost: t.Cost + cost, Paid: t.Paid + paid, Planned: t.Planned || planned}
}

type Ledger struct {
	Version int               `json:"version"`
	State   State             `json:"state"`
	Last    string            `json:"last,omitempty"` // the commit that last changed State
	Commits map[string]Commit `json:"commits,omitempty"`
	Tab     *Tab              `json:"tab,omitempty"`
	path    string
	fresh   bool
}

// keep is how many stamped commits the ledger remembers, for amends.
const keep = 200

func Dir(commonDir string) string { return filepath.Join(commonDir, "git-tab") }

// Open reads a repository's ledger. A missing one is fresh: Fresh() says
// so, and its State is zero.
func Open(commonDir string) (*Ledger, error) {
	l := &Ledger{Version: 1, Commits: map[string]Commit{}, path: filepath.Join(Dir(commonDir), "ledger.json")}
	b, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		l.fresh = true
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, l); err != nil {
		l.fresh = true
		l.State, l.Commits, l.Last = State{}, map[string]Commit{}, ""
	}
	if l.Commits == nil {
		l.Commits = map[string]Commit{}
	}
	return l, nil
}

func (l *Ledger) Fresh() bool { return l.fresh }

func (l *Ledger) Save() error {
	// Only the last commit can have its settlement taken back by an amend.
	for k, c := range l.Commits {
		if k != l.Last && len(c.Before.Marks) > 0 {
			c.Before.Marks = nil
			l.Commits[k] = c
		}
	}
	if len(l.Commits) > keep {
		type kv struct {
			sha string
			end time.Time
		}
		var all []kv
		for k, c := range l.Commits {
			all = append(all, kv{k, c.End})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].end.After(all[j].end) })
		for _, e := range all[keep:] {
			delete(l.Commits, e.sha)
		}
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(l.path, b)
}

// Lock serializes ledger updates between work trees committing at once. A
// lock left by a crashed process is broken after a few seconds.
func Lock(commonDir string) (unlock func()) {
	path := filepath.Join(Dir(commonDir), "ledger.lock")
	os.MkdirAll(filepath.Dir(path), 0o755)
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(path) }
		}
		if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) > 10*time.Second {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Pending is what prepare-commit-msg left for the hooks after it, in this
// work tree's git directory.
type Pending struct {
	Head     string    `json:"head"`               // HEAD when the commit started
	Amend    bool      `json:"amend"`              // the commit replaces Head
	Source   string    `json:"source"`             // prepare-commit-msg's second argument
	Deferred bool      `json:"deferred,omitempty"` // commit-msg is to stamp it
	Stamped  bool      `json:"stamped,omitempty"`  // the receipt is in the message
	Before   State     `json:"before"`
	End      time.Time `json:"end"`
	Threads  []string  `json:"threads,omitempty"`
	Cost     float64   `json:"cost"`
	Paid     float64   `json:"paid"`
	Planned  bool      `json:"planned,omitempty"`
	Tab      *Tab      `json:"tab,omitempty"` // the history's receipts before this one
	Written  time.Time `json:"written"`
}

func pendingPath(gitDir string) string { return filepath.Join(gitDir, "git-tab", "pending.json") }

func ReadPending(gitDir string) (*Pending, bool) {
	b, err := os.ReadFile(pendingPath(gitDir))
	if err != nil {
		return nil, false
	}
	var p Pending
	if json.Unmarshal(b, &p) != nil || time.Since(p.Written) > 24*time.Hour {
		return nil, false
	}
	return &p, true
}

func WritePending(gitDir string, p *Pending) error {
	p.Written = time.Now()
	b, _ := json.Marshal(p)
	return writeAtomic(pendingPath(gitDir), b)
}

func ClearPending(gitDir string) { os.Remove(pendingPath(gitDir)) }

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteFile replaces a file in one step, so a hook stopped halfway leaves
// it whole.
func WriteFile(path string, b []byte) error { return writeAtomic(path, b) }
