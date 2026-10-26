// Package agents reads coding-agent session logs into usage events.
//
// Each agent is an Adapter registered from its own file (claude.go,
// codex.go). Supporting another agent means adding one file whose Adapter
// turns that agent's local logs into Events; nothing else has to change.
package agents

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Usage is one or more model calls' tokens, split the way they are priced.
type Usage struct {
	Input        int64 `json:"input"`          // prompt tokens neither read from nor written to a cache
	CacheRead    int64 `json:"cache_read"`     // prompt tokens read from a cache
	CacheWrite   int64 `json:"cache_write"`    // prompt tokens written to a cache at its default (5-minute) TTL
	CacheWrite1h int64 `json:"cache_write_1h"` // prompt tokens written to Anthropic's 1-hour cache
	Output       int64 `json:"output"`         // completion tokens, reasoning included
}

// Prompt is every input token of the calls, cached or not.
func (u Usage) Prompt() int64 { return u.Input + u.CacheRead + u.CacheWrite + u.CacheWrite1h }

// Written is every token written to a cache, whatever its TTL.
func (u Usage) Written() int64 { return u.CacheWrite + u.CacheWrite1h }

func (u Usage) Zero() bool { return u == Usage{} }

func (u *Usage) Add(v Usage) {
	u.Input += v.Input
	u.CacheRead += v.CacheRead
	u.CacheWrite += v.CacheWrite
	u.CacheWrite1h += v.CacheWrite1h
	u.Output += v.Output
}

type Kind int

const (
	Request Kind = iota // one model call, with its usage
	Prompt              // one message a person typed
)

// Event is one thing that happened in a session.
type Event struct {
	Agent   string // adapter name, e.g. "claude-code"
	Session string // the top-level session; a subagent's events carry its parent's
	Thread  string // the session or subagent thread that produced the event
	Time    time.Time
	Cwd     string
	Kind    Kind
	Model   string
	// Provider is who the agent says it called (deepseek, opencode-go,
	// openrouter, ...) when its log says; the model's maker is assumed
	// otherwise.
	Provider string
	Speed    string // "fast" when billed at Anthropic's fast-mode rate
	Usage    Usage
	// Reported is what the agent itself priced the call at, if it does:
	// the fallback for a model missing from the price table.
	Reported float64
}

// Query narrows a scan. Adapters must return every event in (Since, Until]
// and may skip logs that cannot hold events for Dirs.
type Query struct {
	Since time.Time
	Until time.Time
	Dirs  []string
}

func (q Query) holds(t time.Time) bool {
	return t.After(q.Since) && (q.Until.IsZero() || !t.After(q.Until))
}

type Adapter interface {
	Name() string
	// Roots are the directories the adapter reads, for `git tab doctor`.
	Roots() []string
	Scan(q Query, emit func(Event)) error
}

var adapters []Adapter

func Register(a Adapter) { adapters = append(adapters, a) }

func All() []Adapter { return adapters }

// Scan runs every adapter. One that fails is reported and the rest still run.
func Scan(q Query, emit func(Event)) []error {
	var errs []error
	for _, a := range adapters {
		if err := a.Scan(q, emit); err != nil {
			errs = append(errs, errors.New(a.Name()+": "+err.Error()))
		}
	}
	return errs
}

func home() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

// recentFiles lists the files under root that keep says are logs and that
// were modified after since: an event newer than since can only be in a file
// written after it.
func recentFiles(root string, since time.Time, keep func(path string) bool) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || !keep(path) {
			return nil
		}
		info, err := d.Info()
		if err == nil && info.ModTime().After(since) {
			out = append(out, path)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// eachLine calls fn with every line of a log, of any length, decompressing a
// .zst file on the way. The slice is only valid during the call.
func eachLine(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(path, ".zst") {
		z, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return err
		}
		defer z.Close()
		r = z
	}
	br := bufio.NewReaderSize(r, 1<<20)
	var long []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if long != nil {
			line = append(long, chunk...)
			long = nil
		}
		if line = bytes.TrimRight(line, "\r\n"); len(line) > 0 {
			fn(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			// A truncated .zst frame is a log still being written.
			if strings.HasSuffix(path, ".zst") {
				return nil
			}
			return err
		}
	}
}

// lineTime finds a line's top-level "timestamp" without decoding it, so old
// lines of a long log are skipped at the cost of a byte search. Logs put
// the field last (Claude Code) or first (Codex); nested ones, in tool
// output, come between.
func lineTime(line []byte) (time.Time, bool) {
	key := []byte(`"timestamp":"`)
	try := func(i int) (time.Time, bool) {
		if i < 0 {
			return time.Time{}, false
		}
		rest := line[i+len(key):]
		j := bytes.IndexByte(rest, '"')
		if j < 0 {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, string(rest[:j]))
		return t, err == nil
	}
	if bytes.HasPrefix(line, []byte(`{"timestamp":"`)) {
		return try(1)
	}
	return try(bytes.LastIndex(line, key))
}
