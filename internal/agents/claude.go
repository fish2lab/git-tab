package agents

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code keeps one JSONL transcript per session under
// ~/.claude/projects/<cwd with every non-alphanumeric byte as '-'>/, and a
// subagent's under <session>/subagents/, its lines carrying the parent's
// sessionId. An assistant message is written once per content block, each
// line repeating the message's usage, so a call is counted once per
// message.id + requestId (ccusage, magpie).

func init() { Register(claudeCode{}) }

type claudeCode struct{}

func (claudeCode) Name() string { return "claude-code" }

func (claudeCode) Roots() []string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		var out []string
		for _, d := range strings.Split(v, ",") {
			if d = strings.TrimSpace(d); d != "" {
				out = append(out, filepath.Join(d, "projects"))
			}
		}
		return out
	}
	h := home()
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(h, ".config")
	}
	return []string{filepath.Join(h, ".claude", "projects"), filepath.Join(cfg, "claude", "projects")}
}

// claudeDirName is the name Claude Code gives a project directory.
func claudeDirName(path string) string {
	b := []byte(path)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return string(b)
}

// claudeRelevant says whether a project directory can hold events for one of
// dirs: it is named after the directory, one below it, or one above it (a
// session started higher up can cd in). An unexpected name is read.
func claudeRelevant(project string, dirs []string) bool {
	if len(dirs) == 0 || !strings.HasPrefix(project, "-") {
		return true
	}
	for _, d := range dirs {
		enc := claudeDirName(d)
		if project == enc || strings.HasPrefix(project, enc+"-") || strings.HasPrefix(enc, project+"-") {
			return true
		}
	}
	return false
}

type ccUsage struct {
	Input      int64  `json:"input_tokens"`
	CacheRead  int64  `json:"cache_read_input_tokens"`
	CacheWrite int64  `json:"cache_creation_input_tokens"`
	Output     int64  `json:"output_tokens"`
	Speed      string `json:"speed"`
	Creation   *struct {
		Hour int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type ccAssistant struct {
	IsSidechain bool      `json:"isSidechain"`
	IsAPIError  bool      `json:"isApiErrorMessage"`
	Cwd         string    `json:"cwd"`
	SessionID   string    `json:"sessionId"`
	AgentID     string    `json:"agentId"`
	RequestID   string    `json:"requestId"`
	Timestamp   time.Time `json:"timestamp"`
	Message     struct {
		ID    string   `json:"id"`
		Model string   `json:"model"`
		Usage *ccUsage `json:"usage"`
	} `json:"message"`
}

type ccUser struct {
	IsMeta      bool      `json:"isMeta"`
	IsSidechain bool      `json:"isSidechain"`
	Cwd         string    `json:"cwd"`
	SessionID   string    `json:"sessionId"`
	PromptID    string    `json:"promptId"`
	Timestamp   time.Time `json:"timestamp"`
	Origin      *struct {
		Kind string `json:"kind"`
	} `json:"origin"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

var (
	ccAssistantType = []byte(`"type":"assistant"`)
	ccUserType      = []byte(`"type":"user"`)
	ccUsageKey      = []byte(`"usage":{`)
	ccToolResult    = []byte(`"tool_result"`)
)

func (c claudeCode) Scan(q Query, emit func(Event)) error {
	type call struct {
		ev  Event
		out int64
	}
	var calls []call
	seen := map[string]int{}
	prompts := map[string]bool{}
	for _, root := range c.Roots() {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, p := range projects {
			if !p.IsDir() || !claudeRelevant(p.Name(), q.Dirs) {
				continue
			}
			files := recentFiles(filepath.Join(root, p.Name()), q.Since, func(path string) bool {
				return strings.HasSuffix(path, ".jsonl")
			})
			for _, f := range files {
				eachLine(f, func(line []byte) {
					switch {
					case bytes.Contains(line, ccAssistantType) && bytes.Contains(line, ccUsageKey):
						if t, ok := lineTime(line); ok && !q.holds(t) {
							return
						}
						var l ccAssistant
						if json.Unmarshal(line, &l) != nil || l.Message.Usage == nil || l.IsAPIError ||
							l.Message.Model == "" || l.Message.Model == "<synthetic>" || !q.holds(l.Timestamp) {
							return
						}
						u := l.Message.Usage
						ev := Event{Agent: c.Name(), Session: l.SessionID, Thread: l.SessionID, Time: l.Timestamp, Cwd: l.Cwd,
							Kind: Request, Model: l.Message.Model, Usage: ccTokens(u)}
						if u.Speed == "fast" {
							ev.Speed = "fast"
						}
						if l.AgentID != "" {
							ev.Thread = l.SessionID + "/" + l.AgentID
						}
						key := l.Message.ID + "\x00" + l.RequestID
						if l.Message.ID == "" && l.RequestID == "" {
							calls = append(calls, call{ev, u.Output})
							return
						}
						if i, ok := seen[key]; ok {
							// Keep the fullest revision of the call, at its first time.
							if u.Output > calls[i].out {
								ev.Time = calls[i].ev.Time
								calls[i] = call{ev, u.Output}
							}
							return
						}
						seen[key] = len(calls)
						calls = append(calls, call{ev, u.Output})
					case bytes.Contains(line, ccUserType) && !bytes.Contains(line, ccToolResult):
						if t, ok := lineTime(line); ok && !q.holds(t) {
							return
						}
						var l ccUser
						if json.Unmarshal(line, &l) != nil || l.IsMeta || l.IsSidechain || !q.holds(l.Timestamp) || !ccHuman(l) {
							return
						}
						if l.PromptID != "" {
							if prompts[l.SessionID+l.PromptID] {
								return
							}
							prompts[l.SessionID+l.PromptID] = true
						}
						emit(Event{Agent: c.Name(), Session: l.SessionID, Thread: l.SessionID, Time: l.Timestamp, Cwd: l.Cwd, Kind: Prompt})
					}
				})
			}
		}
	}
	for _, c := range calls {
		if !c.ev.Usage.Zero() {
			emit(c.ev)
		}
	}
	return nil
}

func ccTokens(u *ccUsage) Usage {
	t := Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output}
	if u.Creation != nil {
		hour := min(max(u.Creation.Hour, 0), u.CacheWrite)
		t.CacheWrite1h, t.CacheWrite = hour, u.CacheWrite-hour
	}
	return t
}

// ccHuman says whether a user line is something a person typed. Newer
// Claude Code marks it with origin.kind; older lines are told by content: a
// plain string, or text blocks, but not tool results or command output.
func ccHuman(l ccUser) bool {
	if l.Origin != nil {
		return l.Origin.Kind == "human"
	}
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		s = strings.TrimSpace(s)
		return s != "" && !strings.HasPrefix(s, "<local-command-stdout>") && !strings.HasPrefix(s, "[Request interrupted")
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return false
	}
	text := false
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			return false
		case "text":
			text = true
		}
	}
	return text
}
