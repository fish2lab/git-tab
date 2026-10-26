package agents

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/klauspost/compress/zstd"
)

// DeepSeek Harness (dsh) keeps a session in a folder of its own,
// $DSH_HOME/sessions/<project>/<id>/ (~/.dsh), its events a line each in
// session.jsonl, or zstd frames appended a batch at a time
// (session.jsonl.zstd). A newer format is session.v<N>.jsonl[.zstd]; the
// file of an older one stays behind when a session is carried over, so
// only the latest is read. The first line is the header: the session's id,
// cwd and creation time, and for a subagent's its parent's id
// (parentSession, delegationDepth 1 and more). Then an event per line,
// {type, seq, time (ms), data}: an "assistant/message" is one call, its
// usage input without the cache and output with the reasoning, its model
// the one dsh asked for (data.message.source) and the one that answered
// under the reply's replay state; a "user/message" whose source's kind is
// "user" is a prompt (others are instructions, a plugin's, a subagent's
// report...).
//
// Following magpie: a session forked from another starts with that one's
// events, before format 2 its first seedLength of them and from format 2
// those up to a "session/end-seed" said to be inherited; they are counted
// where they were first written. A subagent bills to the session at the
// top of its parents.

func init() { Register(dsh{}) }

type dsh struct{}

func (dsh) Name() string { return "dsh" }

func (dsh) Roots() []string { return []string{filepath.Join(dshHome(), "sessions")} }

func dshHome() string {
	if d := os.Getenv("DSH_HOME"); d != "" {
		return expandHome(d)
	}
	return filepath.Join(home(), ".dsh")
}

// session.jsonl, session.v3.jsonl.zstd, ...
var dshName = regexp.MustCompile(`^session(?:\.v(\d+))?\.jsonl(\.zstd)?$`)

type dshHead struct {
	Type            string `json:"type"`
	Version         int    `json:"version"`
	ID              string `json:"id"`
	Cwd             string `json:"cwd"`
	ParentSession   string `json:"parentSession"`
	DelegationDepth int    `json:"delegationDepth"`
	SeedLength      int64  `json:"seedLength"`
	IsSeeded        bool   `json:"isSeeded"`
}

type dshEvent struct {
	Type string `json:"type"`
	Seq  *int64 `json:"seq"`
	Time int64  `json:"time"`
	Data struct {
		Inherited bool `json:"inherited"` // session/end-seed
		Source    struct {
			Kind string `json:"kind"`
		} `json:"source"` // user/message
		Message struct {
			Source struct {
				Model       string `json:"model"`
				Provider    string `json:"provider"`
				ReplayState struct {
					Response struct {
						Model         string `json:"model"`
						Provider      string `json:"provider"`
						ResponseModel string `json:"responseModel"`
					} `json:"response"`
				} `json:"replayState"`
			} `json:"source"`
		} `json:"message"`
		Usage *struct {
			Input      int64 `json:"inputTokens"`
			Output     int64 `json:"outputTokens"`
			CacheRead  int64 `json:"cacheReadTokens"`
			CacheWrite int64 `json:"cacheWriteTokens"`
		} `json:"usage"`
	} `json:"data"`
}

var dshWanted = [][]byte{[]byte(`"assistant/message"`), []byte(`"user/message"`), []byte(`"session/end-seed"`)}

// latest is the file of a session folder's latest format, the one written
// last of its two kinds, "" for none.
func (dsh) latest(dir string) (string, os.FileInfo) {
	es, err := os.ReadDir(dir)
	if err != nil {
		return "", nil
	}
	var path string
	var info os.FileInfo
	gen := -1
	for _, e := range es {
		m := dshName.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		fi, err := e.Info()
		if err != nil || !fi.Mode().IsRegular() || n < gen || n == gen && !fi.ModTime().After(info.ModTime()) {
			continue
		}
		path, info, gen = filepath.Join(dir, e.Name()), fi, n
	}
	return path, info
}

// dshReadHead reads a session file's first line, its header.
func dshReadHead(path string) (h dshHead, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return h, false
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".zstd") {
		z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return h, false
		}
		defer z.Close()
		r = z
	}
	line, _ := bufio.NewReaderSize(r, 64<<10).ReadBytes('\n')
	ok = json.Unmarshal(bytes.TrimSpace(line), &h) == nil && h.Type == "session" && h.ID != ""
	return h, ok
}

func (d dsh) Scan(q Query, emit func(Event)) error {
	root := filepath.Join(dshHome(), "sessions")
	dirs, _ := filepath.Glob(filepath.Join(root, "*", "*"))
	heads := map[string]*dshHead{} // by id, nil for one not found
	// parent finds a session's header by its id, for the chain up to the
	// top session: its folder is named after the id
	parent := func(id string) *dshHead {
		if h, ok := heads[id]; ok {
			return h
		}
		heads[id] = nil
		ps, _ := filepath.Glob(filepath.Join(root, "*", dshSegment(id)))
		for _, p := range ps {
			if f, _ := d.latest(p); f != "" {
				if h, ok := dshReadHead(f); ok && h.ID == id {
					heads[id] = &h
					break
				}
			}
		}
		return heads[id]
	}
	for _, dir := range dirs {
		path, info := d.latest(dir)
		if path == "" || !info.ModTime().After(q.Since) {
			continue
		}
		d.file(path, q, parent, emit)
	}
	return nil
}

func (d dsh) file(path string, q Query, parent func(string) *dshHead, emit func(Event)) {
	var (
		h      dshHead
		headed bool
		events []Event
	)
	session := func() string {
		id, cur := h.ID, &h
		for n := 0; cur != nil && cur.DelegationDepth > 0 && cur.ParentSession != "" && n < 64; n++ {
			id, cur = cur.ParentSession, parent(cur.ParentSession)
		}
		return id
	}
	eachLine(path, func(line []byte) {
		if !headed {
			headed = true
			if json.Unmarshal(line, &h) != nil || h.Type != "session" || h.ID == "" {
				h = dshHead{}
			}
			return
		}
		if h.ID == "" {
			return
		}
		wanted := false
		for _, w := range dshWanted {
			if bytes.Contains(line, w) {
				wanted = true
				break
			}
		}
		if !wanted {
			return
		}
		var e dshEvent
		if json.Unmarshal(line, &e) != nil {
			return
		}
		if e.Type == "session/end-seed" && e.Data.Inherited && h.Version >= 2 && h.IsSeeded {
			// what came before is the session this one was forked from
			events = events[:0]
			return
		}
		if h.Version < 2 && e.Seq != nil && *e.Seq < h.SeedLength {
			return
		}
		at := msTime(e.Time)
		if !q.holds(at) {
			return
		}
		ev := Event{Agent: d.Name(), Thread: h.ID, Time: at, Cwd: h.Cwd}
		switch e.Type {
		case "user/message":
			if e.Data.Source.Kind == "user" {
				ev.Kind = Prompt
				events = append(events, ev)
			}
		case "assistant/message":
			u, src := e.Data.Usage, e.Data.Message.Source
			if u == nil || u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
				return
			}
			// bill what answered: the vendor's reply names it when it
			// served another model than the one asked for
			resp := src.ReplayState.Response
			ev.Model = firstOf(resp.ResponseModel, resp.Model, src.Model)
			ev.Provider = firstOf(src.Provider, resp.Provider, "deepseek")
			ev.Usage = Usage{Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output}
			if ev.Model != "" && !ev.Usage.Zero() {
				events = append(events, ev)
			}
		}
	})
	if len(events) == 0 {
		return
	}
	s := session()
	for _, e := range events {
		e.Session = s
		emit(e)
	}
}

// dshSegment is a session id as dsh names its folder (encodeSegment in
// dsh-session-persistence-jsonl, via magpie): A-Z, a-z, 0-9, '.', '_' and
// '-' kept, every other UTF-16 unit ~XXXX, and "." and ".." escaped whole.
func dshSegment(id string) string {
	switch id {
	case ".":
		return "~002E"
	case "..":
		return "~002E~002E"
	}
	var b strings.Builder
	for _, u := range utf16.Encode([]rune(id)) {
		if u < 0x80 && (u >= 'A' && u <= 'Z' || u >= 'a' && u <= 'z' || u >= '0' && u <= '9' || u == '.' || u == '_' || u == '-') {
			b.WriteByte(byte(u))
		} else {
			fmt.Fprintf(&b, "~%04X", u)
		}
	}
	return b.String()
}
