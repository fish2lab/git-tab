package agents

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Codex writes a rollout file per thread under ~/.codex/sessions/YYYY/MM/DD/
// (archived ones under ~/.codex/archived_sessions/), newer versions as
// .jsonl.zst: a session_meta line, a turn_context per turn naming the model
// and cwd, and token_count events carrying the thread's running total and
// the last call's usage. input_tokens includes the cached and cache-written
// tokens. Newer rollouts also write a token_usage_record per response.
//
// Following magpie: a call's own usage is last_token_usage; a repeated
// total is a re-emitted checkpoint, not a call; and a forked thread
// replays its parent's checkpoints with last = 0, which adds nothing.

func init() { Register(codex{}) }

type codex struct{}

func (codex) Name() string { return "codex" }

func (codex) Roots() []string {
	h := os.Getenv("CODEX_HOME")
	if h == "" {
		h = filepath.Join(home(), ".codex")
	}
	return []string{filepath.Join(h, "sessions"), filepath.Join(h, "archived_sessions")}
}

type cxUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

func (u cxUsage) tokens() Usage {
	fresh := u.Input - u.Cached - u.CacheWrite
	if fresh < 0 {
		fresh = 0
	}
	return Usage{Input: fresh, CacheRead: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output}
}

func (u cxUsage) covers(v cxUsage) bool {
	return u.Input >= v.Input && u.Output >= v.Output && u.Cached >= v.Cached && u.CacheWrite >= v.CacheWrite
}

func (u cxUsage) less(v cxUsage) cxUsage {
	return cxUsage{u.Input - v.Input, u.Cached - v.Cached, u.CacheWrite - v.CacheWrite, u.Output - v.Output}
}

type cxLine struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type           string          `json:"type"`
		ID             string          `json:"id"`
		SessionID      string          `json:"session_id"`
		ParentThreadID string          `json:"parent_thread_id"`
		Source         json.RawMessage `json:"source"`
		Cwd            string          `json:"cwd"`
		Model          string          `json:"model"`
		ResponseID     string          `json:"response_id"`
		Usage          *cxUsage        `json:"usage"`
		Settings       *struct {
			Model string `json:"model"`
			Cwd   string `json:"cwd"`
		} `json:"thread_settings"`
		Info *struct {
			Total *cxUsage `json:"total_token_usage"`
			Last  *cxUsage `json:"last_token_usage"`
		} `json:"info"`
	} `json:"payload"`
}

var cxWanted = [][]byte{
	[]byte(`"type":"session_meta"`),
	[]byte(`"type":"turn_context"`),
	[]byte(`"type":"token_usage_record"`),
	[]byte(`"type":"token_count"`),
	[]byte(`"type":"user_message"`),
	[]byte(`"type":"thread_settings_applied"`),
}

func (c codex) Scan(q Query, emit func(Event)) error {
	responses := map[string]bool{}
	for _, root := range c.Roots() {
		files := recentFiles(root, q.Since, func(path string) bool {
			base := filepath.Base(path)
			return strings.HasPrefix(base, "rollout-") && (strings.HasSuffix(base, ".jsonl") || strings.HasSuffix(base, ".jsonl.zst"))
		})
		for _, f := range files {
			c.file(f, q, responses, emit)
		}
	}
	return nil
}

func (c codex) file(path string, q Query, responses map[string]bool, emit func(Event)) {
	var (
		thread, session, cwd, model string
		prev                        *cxUsage
		records, counts             []Event
	)
	ev := func(t time.Time, kind Kind, u Usage) Event {
		return Event{Agent: c.Name(), Session: session, Thread: thread, Time: t, Cwd: cwd, Kind: kind, Model: model, Usage: u}
	}
	eachLine(path, func(line []byte) {
		head := line[:min(len(line), 512)]
		wanted := false
		for _, w := range cxWanted {
			if bytes.Contains(head, w) {
				wanted = true
				break
			}
		}
		if !wanted {
			return
		}
		var l cxLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		p := &l.Payload
		switch {
		case l.Type == "session_meta":
			if thread == "" {
				thread = p.ID
				session = firstOf(p.SessionID, codexParent(p.Source), p.ParentThreadID, p.ID)
			}
			if p.Cwd != "" {
				cwd = p.Cwd
			}
		case l.Type == "turn_context":
			if p.Model != "" {
				model = p.Model
			}
			if p.Cwd != "" {
				cwd = p.Cwd
			}
		case l.Type == "event_msg" && p.Type == "thread_settings_applied" && p.Settings != nil:
			if p.Settings.Model != "" {
				model = p.Settings.Model
			}
			if p.Settings.Cwd != "" {
				cwd = p.Settings.Cwd
			}
		case l.Type == "token_usage_record" && p.Usage != nil:
			key := firstOf(p.SessionID, session) + "\x00" + p.ResponseID
			if p.ResponseID == "" || responses[key] {
				return
			}
			responses[key] = true
			if q.holds(l.Timestamp) {
				records = append(records, ev(l.Timestamp, Request, p.Usage.tokens()))
			}
		case l.Type == "event_msg" && p.Type == "token_count" && p.Info != nil && p.Info.Total != nil:
			total := *p.Info.Total
			if prev != nil && total == *prev {
				return
			}
			var u cxUsage
			switch {
			case p.Info.Last != nil:
				u = *p.Info.Last
			case prev != nil && total.covers(*prev):
				u = total.less(*prev)
			default:
				u = total
			}
			prev = &total
			if t := u.tokens(); !t.Zero() && q.holds(l.Timestamp) {
				counts = append(counts, ev(l.Timestamp, Request, t))
			}
		case l.Type == "event_msg" && p.Type == "user_message":
			if q.holds(l.Timestamp) {
				emit(ev(l.Timestamp, Prompt, Usage{}))
			}
		}
	})
	// A rollout with per-response records counts each call there; its
	// token_count checkpoints describe the same calls.
	calls := counts
	if len(records) > 0 {
		calls = records
	}
	for _, e := range calls {
		emit(e)
	}
}

// codexParent is the thread a subagent was spawned from, given its
// session_meta source ({"subagent": {"thread_spawn": {"parent_thread_id": ...}}}).
func codexParent(source json.RawMessage) string {
	var s struct {
		Subagent struct {
			ThreadSpawn struct {
				Parent string `json:"parent_thread_id"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	}
	if json.Unmarshal(source, &s) != nil {
		return ""
	}
	return s.Subagent.ThreadSpawn.Parent
}

func firstOf(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
