package agents

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Pi (earendil-works/pi) writes a file per session, <time>_<session
// id>.jsonl, in a folder per working directory under its agent folder's
// sessions/ (--<cwd without its leading slash, with / \ and : as '-'>--),
// or all in the one folder PI_CODING_AGENT_SESSION_DIR or the sessionDir
// setting names. The first line is a "session" header naming the id and
// cwd, then an entry per line. A model call is an assistant message naming
// its provider and model, its usage counting input without the cache and
// output with the reasoning, cacheWrite1h the part of cacheWrite kept an
// hour, and cost.total pi's own price for it. Calls outside the
// conversation are "usage" entries (cache_warm, ...) naming theirs, and a
// compaction's, a branch summary's or a tool result's usage, on the model
// in use. A call that failed or was aborted is written with no usage.
//
// omp (oh-my-pi, a fork of Pi) keeps the same files under ~/.omp: a
// "title" line before the header, "model_usage" entries for its own side
// calls, and its subagents' sessions in a folder beside the session's file
// (<time>_<id>/<agent>.jsonl, theirs one folder deeper), which bill to the
// session they ran in.
//
// Following magpie: a session forked from another (its header names a
// parentSession) starts with a copy of that one's entries, ids and times
// kept; they are the parent's calls, written before the fork's header, and
// are left to the parent. A call is counted once per response id, or per
// entry id and time when it has none.

func init() {
	Register(piAgent{name: "pi"})
	Register(piAgent{name: "omp"})
}

type piAgent struct{ name string }

func (p piAgent) Name() string { return p.name }

func (p piAgent) Roots() []string {
	grouped, flat := p.roots()
	return append(grouped, flat...)
}

// roots are the folders holding a folder per working directory, and those
// holding every session directly.
func (p piAgent) roots() (grouped, flat []string) {
	h := home()
	if p.name == "omp" {
		agent := filepath.Join(h, ".omp", "agent")
		grouped = append(grouped, filepath.Join(agent, "sessions"))
		profiles, _ := filepath.Glob(filepath.Join(h, ".omp", "profiles", "*", "agent", "sessions"))
		grouped = append(grouped, profiles...)
		if d := os.Getenv("XDG_DATA_HOME"); d != "" && runtime.GOOS != "windows" {
			grouped = append(grouped, filepath.Join(d, "omp", "sessions"))
		}
		return grouped, nil
	}
	agent := filepath.Join(h, ".pi", "agent")
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		agent = expandHome(d)
	}
	grouped = []string{filepath.Join(agent, "sessions")}
	if d := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); d != "" {
		return grouped, []string{expandHome(d)}
	}
	var s struct {
		SessionDir string `json:"sessionDir"`
	}
	if b, err := os.ReadFile(filepath.Join(agent, "settings.json")); err == nil && json.Unmarshal(b, &s) == nil && s.SessionDir != "" {
		// a relative one is relative to wherever pi was started
		if d := expandHome(s.SessionDir); filepath.IsAbs(d) {
			flat = append(flat, d)
		}
	}
	return grouped, flat
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		return filepath.Join(home(), p[1:])
	}
	return p
}

// piDirName is the folder Pi keeps a working directory's sessions in.
func piDirName(cwd string) string {
	if strings.HasPrefix(cwd, "/") || strings.HasPrefix(cwd, `\`) {
		cwd = cwd[1:]
	}
	return "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(cwd) + "--"
}

// piRelevant says whether a session folder can hold events for one of dirs,
// as claudeRelevant does: named after the directory, one below it or one
// above it. An unexpected name is read.
func piRelevant(folder string, dirs []string) bool {
	inner, ok := strings.CutPrefix(folder, "--")
	inner, ok2 := strings.CutSuffix(inner, "--")
	if len(dirs) == 0 || !ok || !ok2 {
		return true
	}
	for _, d := range dirs {
		enc := strings.TrimSuffix(strings.TrimPrefix(piDirName(d), "--"), "--")
		if inner == enc || strings.HasPrefix(inner, enc+"-") || strings.HasPrefix(enc, inner+"-") {
			return true
		}
	}
	return false
}

type piUsage struct {
	Input        int64  `json:"input"`
	Output       int64  `json:"output"`
	CacheRead    int64  `json:"cacheRead"`
	CacheWrite   int64  `json:"cacheWrite"`
	CacheWrite1h *int64 `json:"cacheWrite1h"`
	Cost         struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func (u *piUsage) tokens(hour1 *int64) Usage {
	var hour int64
	if u.CacheWrite1h != nil {
		hour = *u.CacheWrite1h
	} else if hour1 != nil {
		hour = *hour1
	}
	hour = min(max(hour, 0), u.CacheWrite)
	return Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite - hour, CacheWrite1h: hour, Output: u.Output}
}

type piLine struct {
	Type          string    `json:"type"`
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	Cwd           string    `json:"cwd"`           // session
	ParentSession string    `json:"parentSession"` // session: a fork's or an omp subagent's
	Provider      string    `json:"provider"`      // model_change, usage, model_usage
	ModelID       string    `json:"modelId"`       // model_change
	Model         string    `json:"model"`         // usage, model_usage; omp's model_change, as provider/model
	Usage         *piUsage  `json:"usage"`         // usage, model_usage, compaction, branch_summary
	CacheWrite1h  *int64    `json:"cacheWrite1h"`
	Message       *struct {
		Role       string          `json:"role"`
		Provider   string          `json:"provider"`
		Model      string          `json:"model"`
		ResponseID string          `json:"responseId"`
		Usage      *piUsage        `json:"usage"`
		Content    json.RawMessage `json:"content"` // read for a user's only
	} `json:"message"`
}

// piAssistant is the part of an assistant message the model in use is read
// from.
type piAssistant struct {
	Message struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	} `json:"message"`
}

var (
	piTypeKey = []byte(`{"type":"`)
	piRoleKey = []byte(`"message":{"role":"`)
	piTimeKey = []byte(`"timestamp":"`)
	piUsedKey = []byte(`"usage":{`)
)

// piField is the string a line's first key of the kind holds, without
// decoding the line: pi writes an entry's type first, its timestamp after
// its ids and a message's role first.
func piField(line, key []byte) string {
	i := bytes.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := bytes.IndexByte(rest[:min(len(rest), 64)], '"')
	if j < 0 {
		return ""
	}
	return string(rest[:j])
}

func (p piAgent) Scan(q Query, emit func(Event)) error {
	seen := map[string]bool{}
	read := map[string]bool{}
	file := func(dir, path string) {
		if read[path] {
			return
		}
		read[path] = true
		// an omp subagent's file is under its session's <time>_<id>/,
		// beside <time>_<id>.jsonl
		session := ""
		if rel, err := filepath.Rel(dir, path); err == nil {
			if top, _, ok := strings.Cut(filepath.ToSlash(rel), "/"); ok {
				if _, err := os.Stat(filepath.Join(dir, top+".jsonl")); err == nil {
					_, session, _ = strings.Cut(top, "_")
				}
			}
		}
		p.file(path, session, q, seen, emit)
	}
	keep := func(path string) bool { return strings.HasSuffix(path, ".jsonl") }
	grouped, flat := p.roots()
	for _, root := range grouped {
		folders, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, f := range folders {
			if !f.IsDir() || !piRelevant(f.Name(), q.Dirs) {
				continue
			}
			dir := filepath.Join(root, f.Name())
			for _, path := range recentFiles(dir, q.Since, keep) {
				file(dir, path)
			}
		}
	}
	for _, root := range flat {
		for _, path := range recentFiles(root, q.Since, keep) {
			file(root, path)
		}
	}
	return nil
}

// file reads one session file. parent is the session an omp subagent's
// file bills to, "" for a session's own file.
func (p piAgent) file(path, parent string, q Query, seen map[string]bool, emit func(Event)) {
	var (
		id, cwd           string
		fork              time.Time // a fork's start: what comes before is its parent's
		headed            bool
		provider, model   string // in use, for usage that names none
		lastAssistant     []byte // the latest assistant line not decoded, which names the model in use
		lastAssistantSeen bool
	)
	inUse := func() (string, string) {
		if lastAssistantSeen {
			var a piAssistant
			if json.Unmarshal(lastAssistant, &a) == nil && a.Message.Model != "" {
				provider, model = a.Message.Provider, a.Message.Model
			}
			lastAssistantSeen = false
		}
		return provider, model
	}
	// counted says whether an entry of this time is this session's to count.
	counted := func(t time.Time) bool { return q.holds(t) && !t.Before(fork) }
	call := func(l *piLine, u *piUsage, prov, mod, responseID string) {
		if u == nil || mod == "" || u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 || !counted(l.Timestamp) {
			return
		}
		tok := u.tokens(l.CacheWrite1h)
		if tok.Zero() {
			return
		}
		var key string
		switch {
		case responseID != "":
			// the usage too: a local server may hand out few distinct ids
			key = "r\x00" + responseID + "\x00" + strconv.FormatInt(tok.Prompt(), 10) + "/" + strconv.FormatInt(tok.Output, 10)
		case l.ID != "":
			key = "e\x00" + l.ID + "\x00" + strconv.FormatInt(l.Timestamp.UnixNano(), 10)
		}
		if key != "" {
			if seen[key] {
				return
			}
			seen[key] = true
		}
		emit(Event{Agent: p.name, Session: firstOf(parent, id), Thread: id, Time: l.Timestamp, Cwd: cwd, Kind: Request,
			Model: mod, Provider: prov, Usage: tok, Reported: u.Cost.Total})
	}
	eachLine(path, func(line []byte) {
		typ := ""
		if bytes.HasPrefix(line, piTypeKey) {
			typ = piField(line, piTypeKey)
		}
		if !headed {
			if typ != "session" && !(typ == "" && bytes.Contains(line, []byte(`"type":"session"`))) {
				return // omp's title line, or not a session's file
			}
			var l piLine
			if json.Unmarshal(line, &l) != nil || l.Type != "session" {
				return
			}
			headed, cwd = true, l.Cwd
			id = l.ID
			if id == "" {
				_, id, _ = strings.Cut(strings.TrimSuffix(filepath.Base(path), ".jsonl"), "_")
			}
			if l.ParentSession != "" {
				fork = l.Timestamp
			}
			return
		}
		// what counts is decoded; an entry outside the window only if it
		// says the model in use
		var t time.Time
		if s := piField(line, piTimeKey); s != "" {
			t, _ = time.Parse(time.RFC3339Nano, s)
		}
		late := !t.IsZero() && !counted(t)
		role := ""
		if typ == "message" {
			role = piField(line, piRoleKey)
		}
		switch {
		case typ == "model_change", typ == "message" && role == "":
		case typ == "message" && role == "assistant":
			if late {
				lastAssistant, lastAssistantSeen = append(lastAssistant[:0], line...), true
				return
			}
		case typ == "message" && role == "user":
			if late || parent != "" {
				return
			}
		case typ == "message", typ == "usage", typ == "model_usage", typ == "compaction", typ == "branch_summary":
			if late || !bytes.Contains(line, piUsedKey) {
				return
			}
		case typ != "":
			return
		}
		var l piLine
		if json.Unmarshal(line, &l) != nil {
			return
		}
		switch l.Type {
		case "model_change":
			prov, mod := l.Provider, l.ModelID
			if mod == "" {
				prov, mod, _ = strings.Cut(l.Model, "/")
			}
			if mod != "" {
				provider, model, lastAssistantSeen = prov, mod, false
			}
		case "usage", "model_usage":
			call(&l, l.Usage, l.Provider, l.Model, "")
		case "compaction", "branch_summary":
			if l.Usage != nil {
				prov, mod := inUse()
				call(&l, l.Usage, prov, mod, "")
			}
		case "message":
			m := l.Message
			if m == nil {
				return
			}
			switch m.Role {
			case "assistant":
				if m.Model != "" {
					provider, model, lastAssistantSeen = m.Provider, m.Model, false
				}
				call(&l, m.Usage, m.Provider, m.Model, m.ResponseID)
			case "user":
				if parent != "" || !counted(l.Timestamp) || !piTyped(m.Content) {
					return
				}
				if l.ID != "" {
					key := "p\x00" + l.ID + "\x00" + strconv.FormatInt(l.Timestamp.UnixNano(), 10)
					if seen[key] {
						return
					}
					seen[key] = true
				}
				emit(Event{Agent: p.name, Session: id, Thread: id, Time: l.Timestamp, Cwd: cwd, Kind: Prompt})
			default:
				// a tool that did model work of its own, on the model in use
				if m.Usage != nil {
					prov, mod := inUse()
					call(&l, m.Usage, prov, mod, "")
				}
			}
		}
	})
}

// piTyped says whether a user message is something a person typed: words,
// or a skill called up with /skill:name (sent as <skill name="...">), but
// not what an extension or a template put in wrapped in tags (magpie).
func piTyped(content json.RawMessage) bool {
	var text string
	if json.Unmarshal(content, &text) != nil {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(content, &parts) != nil {
			return false
		}
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		text = b.String()
	}
	if strings.HasPrefix(text, `<skill name="`) {
		return true
	}
	t := strings.TrimSpace(text)
	return t != "" && !strings.HasPrefix(t, "<")
}
