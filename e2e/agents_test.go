// End-to-end tests of the pi, omp, OpenCode and DeepSeek Harness adapters:
// session logs as each agent writes them, in the fake home, billed by the
// real hooks.
package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	_ "modernc.org/sqlite"
)

// ordered is a JSON object with its keys in the order given, as pi and dsh
// write theirs (JSON.stringify keeps insertion order).
func ordered(kv ...any) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv[i])
		v, _ := json.Marshal(kv[i+1])
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes()
}

func wantTrailer(t *testing.T, w *world, key, want string) {
	t.Helper()
	if got := strings.Join(w.trailers("HEAD")[key], " | "); got != want {
		t.Errorf("%s = %q, want %q\n%s", key, got, want, w.message("HEAD"))
	}
}

// --- pi and omp ----------------------------------------------------------------

// piU is a pi usage record: input without the cache, output with the
// reasoning, write1h the part of write kept an hour, cost pi's own price.
type piU struct {
	in, out, read, write, write1h int64
	cost                          float64
}

func (u piU) json() json.RawMessage {
	return ordered("input", u.in, "output", u.out, "cacheRead", u.read, "cacheWrite", u.write, "totalTokens", u.in+u.out+u.read+u.write,
		"cost", ordered("input", 0, "output", 0, "cacheRead", 0, "cacheWrite", 0, "total", u.cost), "cacheWrite1h", u.write1h, "reasoning", 0)
}

// piFile is where pi keeps a session begun at start in cwd, under an agent
// folder (~/.pi/agent, ~/.omp/agent).
func piFile(agentDir, cwd, id string, start time.Time) string {
	dir := "--" + strings.NewReplacer("/", "-", `\`, "-", ":", "-").Replace(strings.TrimPrefix(cwd, "/")) + "--"
	name := strings.NewReplacer(":", "-", ".", "-").Replace(ts(start)) + "_" + id + ".jsonl"
	return filepath.Join(agentDir, "sessions", dir, name)
}

func piHeader(id, cwd string, at time.Time, parent string) string {
	if parent != "" {
		return string(ordered("type", "session", "version", 3, "id", id, "timestamp", ts(at), "cwd", cwd, "parentSession", parent))
	}
	return string(ordered("type", "session", "version", 3, "id", id, "timestamp", ts(at), "cwd", cwd))
}

func piEntry(typ, id string, at time.Time, rest ...any) string {
	return string(ordered(append([]any{"type", typ, "id", id, "parentId", nil, "timestamp", ts(at)}, rest...)...))
}

func piUser(id string, at time.Time, text string) string {
	return piEntry("message", id, at, "message", ordered("role", "user",
		"content", []any{map[string]any{"type": "text", "text": text}}, "timestamp", at.UnixMilli()))
}

func piAssistant(id string, at time.Time, provider, model, response string, u piU) string {
	stop := "toolUse"
	if u == (piU{}) {
		stop = "aborted"
	}
	return piEntry("message", id, at, "message", ordered("role", "assistant",
		"content", []any{map[string]any{"type": "text", "text": `see "timestamp":"2020-01-01T00:00:00.000Z" in the log`}},
		"api", "anthropic-messages", "provider", provider, "model", model, "usage", u.json(), "stopReason", stop,
		"timestamp", at.UnixMilli(), "responseId", response, "durationMs", 1200))
}

// Pi: a session file also kept in the folder PI_CODING_AGENT_SESSION_DIR
// names is billed once; a cache_warm usage entry is a call; an aborted call
// is none; a session forked here from another project's starts with that
// one's entries, which are not this repository's. Opus 5.5 at $4 input,
// $0.20 cache read, $5 5-minute and $8 1-hour cache write, $20 output.
//
//	A (P1):    10 in, 200k read, 30k 1h write, 4k out = $0.36004
//	warm (P1):  2 in, 100k read,               1 out = $0.020028
//	B (fork):   5 in, 230k read,  5k 5m write, 1k out = $0.09102
func TestPi(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	agent := filepath.Join(w.home, ".pi", "agent")
	now := time.Now()
	start := now.Add(-30 * time.Minute)
	p1 := piFile(agent, w.repo, "P1", start)
	appendFile(t, p1, piHeader("P1", w.repo, start, ""),
		piUser("a1", start.Add(time.Minute), "fix the parser"),
		piAssistant("a2", start.Add(2*time.Minute), "anthropic", "claude-opus-5-5", "msg_A", piU{in: 10, read: 200000, write: 30000, write1h: 30000, out: 4000, cost: 0.36004}),
		piAssistant("a3", start.Add(3*time.Minute), "anthropic", "claude-opus-5-5", "", piU{}),
		piEntry("usage", "a4", start.Add(15*time.Minute), "kind", "cache_warm", "provider", "anthropic", "model", "claude-opus-5-5",
			"usage", piU{in: 2, out: 1, read: 100000, cost: 0.020028}.json()))
	// the same session, also in the folder pi was told to keep sessions in
	flat := filepath.Join(w.home, "pi-sessions")
	os.MkdirAll(flat, 0o755)
	b, _ := os.ReadFile(p1)
	os.WriteFile(filepath.Join(flat, filepath.Base(p1)), b, 0o644)
	w.extra = []string{"PI_CODING_AGENT_SESSION_DIR=" + flat}
	// another project's session never counts, nor do its entries that a
	// fork of it made here starts with
	other := filepath.Join(w.home, "work", "other")
	o1 := piFile(agent, other, "O1", start)
	copied := []string{
		piUser("c1", start.Add(time.Minute), "look around"),
		piAssistant("c2", start.Add(2*time.Minute), "anthropic", "claude-opus-5-5", "msg_C", piU{in: 1000000, out: 1000000}),
	}
	appendFile(t, o1, append([]string{piHeader("O1", other, start, "")}, copied...)...)
	forkAt := start.Add(10 * time.Minute)
	appendFile(t, piFile(agent, w.repo, "F1", forkAt), append(append([]string{piHeader("F1", w.repo, forkAt, o1)}, copied...),
		piUser("b1", forkAt.Add(time.Minute), "now fix it here"),
		piAssistant("b2", forkAt.Add(2*time.Minute), "anthropic", "claude-opus-5-5", "msg_B", piU{in: 5, read: 230000, write: 5000, out: 1000, cost: 0.09102}))...)

	w.commit(nil, "-m", "pi work")
	wantTrailer(t, w, "AI-Cost", "$0.471 (api-equiv)")
	wantTrailer(t, w, "AI-Tokens", "in=565k cached=530k written=35k out=5k")
	wantTrailer(t, w, "AI-Agent", "pi")
	wantTrailer(t, w, "AI-Turns", "2")

	// A compaction is billed on the model in use, which the last reply,
	// from before this commit's window, names: 50k in, 5k out = $0.30. A
	// model the price table lacks is billed at pi's own price.
	appendFile(t, p1,
		piEntry("compaction", "a5", time.Now(), "summary", "so far", "firstKeptEntryId", "a2", "tokensBefore", 300000,
			"usage", piU{in: 50000, out: 5000, cost: 0.3}.json()),
		piAssistant("a6", time.Now(), "bonsai", "bonsai-local-9", "local-1", piU{in: 1000, out: 10, cost: 0.05}))
	w.commit(nil, "-m", "after compaction")
	wantTrailer(t, w, "AI-Cost", "$0.350 (api-equiv)")
}

// A commit pi makes, told by PI_SESSION_ID, settles that session only; the
// other pi session in the repository waits for the next commit.
func TestPiPreciseSession(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	agent := filepath.Join(w.home, ".pi", "agent")
	now := time.Now()
	for _, s := range []struct {
		id      string
		in, out int64
	}{{"S1", 10000, 1000}, {"S2", 20000, 2000}} { // $0.06, $0.12
		start := now.Add(-5 * time.Minute)
		appendFile(t, piFile(agent, w.repo, s.id, start), piHeader(s.id, w.repo, start, ""),
			piUser(s.id+"-1", start.Add(time.Second), "go"),
			piAssistant(s.id+"-2", start.Add(time.Minute), "anthropic", "claude-opus-5-5", "msg_"+s.id, piU{in: s.in, out: s.out}))
	}
	w.commit([]string{"PI_SESSION_ID=S1"}, "-m", "pi's commit")
	wantTrailer(t, w, "AI-Cost", "$0.060 (api-equiv)")
	w.commit(nil, "-m", "person's commit")
	wantTrailer(t, w, "AI-Cost", "$0.120 (api-equiv)")
	if msg := w.commit(nil, "-m", "nothing new"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("billed again:\n%s", msg)
	}
}

// omp: a title line before the header, a model_usage side call, and a
// subagent's session beside the session's file, billed to it; a task
// tool's result repeats the subagent's usage and is not billed again.
//
//	reply (opus 5.5):    10k in, 1k out = $0.06
//	title (haiku 5.5):   20k in, 2k out = $0.003
//	subagent (sonnet 5.5): 10k in, 1k out = $0.03
func TestOmp(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	agent := filepath.Join(w.home, ".omp", "agent")
	start := time.Now().Add(-10 * time.Minute)
	main := piFile(agent, w.repo, "M1", start)
	appendFile(t, main, string(ordered("type", "title", "v", 1, "title", "Port the parser", "source", "auto", "pad", strings.Repeat(" ", 80))),
		piHeader("M1", w.repo, start, ""),
		piEntry("model_change", "m0", start, "model", "anthropic/claude-opus-5-5", "role", "default"),
		piUser("m1", start.Add(time.Second), "port the parser"),
		piAssistant("m2", start.Add(time.Minute), "anthropic", "claude-opus-5-5", "msg_M", piU{in: 10000, out: 1000}),
		piEntry("model_usage", "m3", start.Add(time.Minute), "purpose", "title", "role", "smol", "provider", "anthropic",
			"model", "claude-haiku-5-5", "usage", piU{in: 20000, out: 2000}.json()),
		piEntry("message", "m4", start.Add(3*time.Minute), "message", ordered("role", "toolResult", "toolName", "task",
			"content", []any{map[string]any{"type": "text", "text": "done"}}, "details", map[string]any{"usage": piU{in: 10000, out: 1000}.json()})))
	sub := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "0-Explore.jsonl")
	appendFile(t, sub, piHeader("X1", w.repo, start.Add(2*time.Minute), main),
		piUser("x1", start.Add(2*time.Minute), "find the parser"),
		piAssistant("x2", start.Add(2*time.Minute+30*time.Second), "anthropic", "claude-sonnet-5-5", "msg_X", piU{in: 10000, out: 1000}))
	// another omp session in the repository, for the person's commit
	appendFile(t, piFile(agent, w.repo, "M2", start), piHeader("M2", w.repo, start, ""),
		piAssistant("n1", start.Add(time.Minute), "anthropic", "claude-opus-5-5", "msg_N", piU{in: 20000, out: 2000}))

	w.commit([]string{"GIT_TAB_SESSION=omp:M1"}, "-m", "omp's commit")
	wantTrailer(t, w, "AI-Cost", "$0.093 (api-equiv)")
	wantTrailer(t, w, "AI-Agent", "omp")
	wantTrailer(t, w, "AI-Turns", "1")
	w.commit(nil, "-m", "person's commit")
	wantTrailer(t, w, "AI-Cost", "$0.120 (api-equiv)")
}

// --- OpenCode -------------------------------------------------------------------

// OpenCode 2's tables, as opencode 2.1.4 creates them, and the 1.2 ones it
// leaves behind once it has copied them over.
const openCodeSchema = `
CREATE TABLE session_v2 (
	id text PRIMARY KEY, project_id text NOT NULL, workspace_id text, parent_id text, fork_session_id text, fork_boundary text,
	slug text NOT NULL, directory text NOT NULL, path text, title text, version text NOT NULL, share_url text,
	summary_additions integer, summary_deletions integer, summary_files integer, summary_diffs text, metadata text,
	cost real DEFAULT 0 NOT NULL, tokens_input integer DEFAULT 0 NOT NULL, tokens_output integer DEFAULT 0 NOT NULL,
	tokens_reasoning integer DEFAULT 0 NOT NULL, tokens_cache_read integer DEFAULT 0 NOT NULL, tokens_cache_write integer DEFAULT 0 NOT NULL,
	revert text, permission text, agent text, model text, time_created integer NOT NULL, time_updated integer NOT NULL,
	time_idle integer, time_viewed integer, idle_outcome text, time_compacting integer, time_archived integer,
	time_suspended integer, resume_attempts integer DEFAULT 0 NOT NULL);
CREATE INDEX session_v2_parent_idx ON session_v2 (parent_id);
CREATE TABLE session_message (
	id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL,
	time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id, seq);
CREATE TABLE kv (key text PRIMARY KEY, value text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE session (id text PRIMARY KEY, project_id text, parent_id text, directory text NOT NULL, title text,
	time_created integer NOT NULL, time_updated integer NOT NULL);
CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
	time_updated integer NOT NULL, data text NOT NULL);
`

type ocTok struct{ in, out, reasoning, read, write int64 }

// OpenCode 2, read from its database while it is open in WAL mode and
// after it is closed: a subagent's session bills to the session it ran
// in, the reasoning is output, a reply cut short is no call, and the old
// tables a finished migration left behind are not read. Opus 5.5:
//
//	root:     1k in, 100k read, 10k write, 2k out + 3k reasoning = $0.174
//	subagent: 500 in, 100 out + 400 reasoning                    = $0.012
//	other:    10k in, 1k out                                     = $0.060
func TestOpenCode(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	path := filepath.Join(w.home, ".local", "share", "opencode", "opencode.db")
	os.MkdirAll(filepath.Dir(path), 0o755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`PRAGMA journal_mode=WAL`)
	exec(openCodeSchema)
	now := time.Now()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	session := func(id, parent, dir string) {
		var p any
		if parent != "" {
			p = parent
		}
		exec(`INSERT INTO session_v2 (id, project_id, parent_id, slug, directory, version, time_created, time_updated)
			VALUES (?, 'global', ?, ?, ?, '2.1.4', ?, ?)`, id, p, id, dir, ms(-20*time.Minute), ms(0))
	}
	seq := 0
	message := func(sid, typ string, at int64, data any) {
		seq++
		b, _ := json.Marshal(data)
		exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			"msg_"+string(rune('a'+seq)), sid, typ, seq, at, at+1000, string(b))
	}
	reply := func(sid string, at int64, tok *ocTok, cost float64) {
		data := map[string]any{"time": map[string]any{"created": at, "completed": at + 900}, "agent": "build",
			"model": map[string]any{"id": "claude-opus-5-5", "providerID": "anthropic", "variant": "high"}, "finish": "tool-calls", "cost": cost}
		if tok != nil {
			data["tokens"] = map[string]any{"input": tok.in, "output": tok.out, "reasoning": tok.reasoning,
				"cache": map[string]any{"read": tok.read, "write": tok.write}}
		} else {
			data["finish"], data["error"] = "error", map[string]any{"type": "aborted", "message": "Step interrupted"}
		}
		message(sid, "assistant", at, data)
	}
	prompt := func(sid string, at int64, text string) {
		message(sid, "user", at, map[string]any{"time": map[string]any{"created": at}, "text": text, "files": []any{}, "agents": []any{}})
	}
	session("ses_root", "", w.repo)
	session("ses_sub", "ses_root", w.repo)
	session("ses_other", "", w.repo)
	session("ses_away", "", filepath.Join(w.home, "work", "other"))
	prompt("ses_root", ms(-10*time.Minute), "add a parser")
	reply("ses_root", ms(-9*time.Minute), &ocTok{in: 1000, out: 2000, reasoning: 3000, read: 100000, write: 10000}, 0.174)
	prompt("ses_sub", ms(-8*time.Minute), "find the parser") // the parent's instructions, no turn
	reply("ses_sub", ms(-8*time.Minute), &ocTok{in: 500, out: 100, reasoning: 400}, 0.012)
	reply("ses_root", ms(-7*time.Minute), nil, 0)
	reply("ses_other", ms(-6*time.Minute), &ocTok{in: 10000, out: 1000}, 0.06)
	reply("ses_away", ms(-6*time.Minute), &ocTok{in: 1000000, out: 1000000}, 99)
	// the tables OpenCode 2 copied its sessions from, left behind: one it
	// copied, and one deleted since in OpenCode 2
	exec(`INSERT INTO kv VALUES ('migration.v1-v2', '{"phase":"completed"}', ?, ?)`, ms(-11*time.Minute), ms(-11*time.Minute))
	old, _ := json.Marshal(map[string]any{"role": "assistant", "modelID": "claude-opus-5-5", "providerID": "anthropic",
		"time":   map[string]any{"created": ms(-12 * time.Minute), "completed": ms(-12 * time.Minute)},
		"tokens": map[string]any{"input": 999999, "output": 999999, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}}})
	for _, id := range []string{"ses_root", "ses_gone"} {
		exec(`INSERT INTO session VALUES (?, 'global', NULL, ?, 'old', ?, ?)`, id, w.repo, ms(-20*time.Minute), ms(-12*time.Minute))
		exec(`INSERT INTO message VALUES (?, ?, ?, ?, ?)`, "old_"+id, id, ms(-12*time.Minute), ms(-12*time.Minute), string(old))
	}
	if _, err := os.Stat(path + "-wal"); err != nil {
		t.Fatalf("no WAL while OpenCode has the database open: %v", err)
	}

	w.commit([]string{"OPENCODE_SESSION_ID=ses_root"}, "-m", "opencode's commit")
	wantTrailer(t, w, "AI-Cost", "$0.186 (api-equiv)")
	wantTrailer(t, w, "AI-Tokens", "in=112k cached=100k written=10k out=5.5k")
	wantTrailer(t, w, "AI-Agent", "opencode")
	wantTrailer(t, w, "AI-Turns", "1")

	// OpenCode quit: its log checkpointed into the database and removed
	db.Close()
	if _, err := os.Stat(path + "-wal"); err == nil {
		t.Fatalf("the WAL outlived the last connection")
	}
	w.commit(nil, "-m", "person's commit")
	wantTrailer(t, w, "AI-Cost", "$0.060 (api-equiv)")
	for _, f := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("reading the closed database made %s", filepath.Base(f))
		}
	}
}

// --- DeepSeek Harness ------------------------------------------------------------

func dshEvent(typ string, seq int, at time.Time, data any) string {
	return string(ordered("type", typ, "seq", seq, "time", at.UnixMilli(), "data", data))
}

func dshReply(seq int, at time.Time, model, served string, in, out, read int64) string {
	src := ordered("model", model, "provider", "deepseek", "replayState", ordered("response",
		ordered("provider", "deepseek", "model", model, "responseModel", served)))
	return dshEvent("assistant/message", seq, at, ordered("message", ordered("role", "assistant", "source", src),
		"usage", ordered("inputTokens", in, "outputTokens", out, "cacheReadTokens", read, "cacheWriteTokens", 0, "reasoningTokens", 0)))
}

func dshPrompt(seq int, at time.Time, kind, text string) string {
	return dshEvent("user/message", seq, at, ordered("content", text, "source", ordered("kind", kind)))
}

// writeDsh writes a session file, zstd frames of up to two lines each when
// packed, as dsh appends a batch at a time.
func writeDsh(t *testing.T, path string, packed bool, lines ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	var data []byte
	if !packed {
		data = []byte(strings.Join(lines, "\n") + "\n")
	} else {
		enc, _ := zstd.NewWriter(nil)
		for i := 0; i < len(lines); i += 2 {
			data = enc.EncodeAll([]byte(strings.Join(lines[i:min(i+2, len(lines))], "\n")+"\n"), data)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// DeepSeek Harness: a packed session (.zstd) carried over to format 3, with
// its format-0 file left beside it, and its subagent, which bills to it;
// two forks, seeded with its first prompt and reply, which are not billed
// again, one asking for V4 Pro and served V4 Flash. V4 Flash at DeepSeek's
// idle rate (night in Beijing): $0.15 input, $0.003 cache read, $0.60
// output.
//
//	dsh-a:     100k in, 1M read, 20k out = $0.030
//	dsh-b:      50k in,           6k out = $0.0111
//	dsh-c:     200k in,          12k out = $0.0372
//	dsh-d:       1k in,          100 out = $0.00021
func TestDsh(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	beijing := time.FixedZone("CST", 8*3600)
	b := time.Now().In(beijing)
	at := time.Date(b.Year(), b.Month(), b.Day(), 3, 0, 0, 0, beijing)
	if at.Add(time.Hour).After(time.Now()) {
		at = at.AddDate(0, 0, -1)
	}
	sessions := filepath.Join(w.home, ".dsh", "sessions", "work-repo-3f2a")
	head := func(id string, version int, extra ...any) string {
		return string(ordered(append([]any{"type", "session", "version", version, "id", id, "cwd", w.repo, "createdAt", at.UnixMilli()}, extra...)...))
	}
	seed := []string{
		dshPrompt(1, at.Add(time.Minute), "user", "tidy the build script"),
		dshReply(2, at.Add(2*time.Minute), "deepseek-v4-flash", "deepseek-v4-flash", 100000, 20000, 1000000),
	}
	writeDsh(t, filepath.Join(sessions, "dsh-a", "session.jsonl"), false, head("dsh-a", 0),
		dshReply(0, at.Add(time.Minute), "deepseek-v4-flash", "", 999999, 999999, 0))
	writeDsh(t, filepath.Join(sessions, "dsh-a", "session.v3.jsonl.zstd"), true, append([]string{head("dsh-a", 3),
		dshPrompt(0, at, "instructions", "follow the house rules")}, append(seed,
		dshEvent("session/title", 3, at.Add(3*time.Minute), ordered("title", "Build script tidy")),
		string(ordered("rows", [][]any{{1, "x"}})))...)...)
	writeDsh(t, filepath.Join(sessions, "dsh-b", "session.v3.jsonl"), false,
		head("dsh-b", 3, "parentSession", "dsh-a", "delegationDepth", 1),
		dshPrompt(0, at.Add(4*time.Minute), "agent", "look at the tests"),
		dshReply(1, at.Add(5*time.Minute), "deepseek-v4-flash", "deepseek-v4-flash", 50000, 6000, 0))
	writeDsh(t, filepath.Join(sessions, "dsh-c", "session.v3.jsonl.zstd"), true, append(append([]string{
		head("dsh-c", 3, "isSeeded", true)}, seed...),
		dshEvent("session/end-seed", 3, at.Add(10*time.Minute), ordered("inherited", true)),
		dshPrompt(4, at.Add(11*time.Minute), "user", "now the tests"),
		dshReply(5, at.Add(12*time.Minute), "deepseek-v4-pro", "deepseek-v4-flash", 200000, 12000, 0))...)
	writeDsh(t, filepath.Join(sessions, "dsh-d", "session.jsonl"), false, append(append([]string{
		head("dsh-d", 0, "seedLength", 2)}, strings.Replace(seed[0], `"seq":1`, `"seq":0`, 1), strings.Replace(seed[1], `"seq":2`, `"seq":1`, 1)),
		dshReply(2, at.Add(20*time.Minute), "deepseek-v4-flash", "", 1000, 100, 0))...)

	w.commit([]string{"GIT_TAB_SESSION=dsh:dsh-a"}, "-m", "dsh-a's commit")
	wantTrailer(t, w, "AI-Cost", "$0.041 (api-equiv)")
	wantTrailer(t, w, "AI-Agent", "dsh")
	wantTrailer(t, w, "AI-Turns", "1")
	w.commit(nil, "-m", "the forks' commit")
	wantTrailer(t, w, "AI-Cost", "$0.037 (api-equiv)")
	wantTrailer(t, w, "AI-Model", "deepseek-v4-flash")
	wantTrailer(t, w, "AI-Turns", "1")
}
