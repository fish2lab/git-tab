// End-to-end tests: the real binary, real git and real hooks, against a
// fake home directory holding Claude Code and Codex session logs.
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "git-tab-e2e-bin")
	if err != nil {
		panic(err)
	}
	binDir = dir
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "git-tab"), "..")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// world is a fake home with one repository in it.
type world struct {
	t     *testing.T
	home  string
	repo  string
	file  string // the file commit changes
	env   []string
	extra []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, home: home, repo: filepath.Join(home, "work", "repo")}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "CLAUDE") || strings.HasPrefix(k, "CODEX") ||
			strings.HasPrefix(k, "PI_") || strings.HasPrefix(k, "OPENCODE") || strings.HasPrefix(k, "DSH_") ||
			k == "HOME" || k == "XDG_CONFIG_HOME" || k == "XDG_CACHE_HOME" || k == "PATH" ||
			k == "LANG" || k == "LC_ALL" || k == "LC_MESSAGES" {
			continue
		}
		w.env = append(w.env, kv)
	}
	w.env = append(w.env, "LANG=en_US.UTF-8", "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"XDG_CACHE_HOME="+filepath.Join(home, ".cache"), "CODEX_HOME="+filepath.Join(home, ".codex"),
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"PATH="+binDir+":"+os.Getenv("PATH"))
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(`[user]
	name = Test
	email = test@example.com
[init]
	defaultBranch = main
[commit]
	gpgsign = false
[tab]
	autoUpdatePrices = false
`), 0o644)
	os.MkdirAll(w.repo, 0o755)
	w.git("init", "-q")
	return w
}

func (w *world) run(dir string, env []string, name string, args ...string) (string, error) {
	if name == "git-tab" {
		name = filepath.Join(binDir, name)
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(append(append([]string{}, w.env...), w.extra...), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func (w *world) git(args ...string) string {
	w.t.Helper()
	out, err := w.run(w.repo, nil, "git", args...)
	if err != nil {
		w.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

func (w *world) tab(args ...string) string {
	w.t.Helper()
	out, err := w.run(w.repo, nil, "git-tab", args...)
	if err != nil {
		w.t.Fatalf("git tab %v: %v\n%s", args, err, out)
	}
	return out
}

// commit stages a change and commits it with extra git arguments.
func (w *world) commit(env []string, args ...string) string {
	w.t.Helper()
	f := filepath.Join(w.repo, firstOr(w.file, "file.txt"))
	b, _ := os.ReadFile(f)
	os.WriteFile(f, append(b, []byte(fmt.Sprintf("%d\n", time.Now().UnixNano()))...), 0o644)
	w.git("add", "-A")
	out, err := w.run(w.repo, env, "git", append([]string{"commit", "-q"}, args...)...)
	if err != nil {
		w.t.Fatalf("git commit %v: %v\n%s", args, err, out)
	}
	return w.message("HEAD")
}

func (w *world) message(rev string) string { return w.git("log", "-1", "--format=%B", rev) }

// trailers are what git itself parses out of a commit's message.
func (w *world) trailers(rev string) map[string][]string {
	out := w.git("log", "-1", "--format=%(trailers:unfold)", rev)
	m := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if k, v, ok := strings.Cut(l, ": "); ok {
			m[k] = append(m[k], v)
		}
	}
	return m
}

// --- session log fixtures ---------------------------------------------------

type cc struct {
	id, req, model                string
	in, read, write, write1h, out int64
	speed                         string
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func claudeDir(home, cwd string) string {
	enc := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(cwd, "-")
	return filepath.Join(home, ".claude", "projects", enc)
}

func appendFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// assistant writes one Claude Code call, as blocks lines repeating its usage.
func (w *world) assistant(session, cwd string, at time.Time, c cc, blocks int) {
	usage := map[string]any{"input_tokens": c.in, "cache_read_input_tokens": c.read, "cache_creation_input_tokens": c.write + c.write1h,
		"output_tokens": c.out, "cache_creation": map[string]any{"ephemeral_1h_input_tokens": c.write1h, "ephemeral_5m_input_tokens": c.write},
		"service_tier": "standard", "speed": firstOr(c.speed, "standard")}
	if c.model == "" {
		c.model = "claude-opus-5-5"
	}
	var lines []string
	for i := 0; i < blocks; i++ {
		lines = append(lines, mustJSON(map[string]any{"parentUuid": nil, "isSidechain": false, "type": "assistant",
			"message":   map[string]any{"id": c.id, "model": c.model, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "ok"}}, "usage": usage},
			"requestId": c.req, "uuid": fmt.Sprintf("%s-%d", c.id, i), "timestamp": ts(at.Add(time.Duration(i) * time.Millisecond)),
			"cwd": cwd, "sessionId": session, "version": "2.1.308"}))
	}
	appendFile(w.t, filepath.Join(claudeDir(w.home, cwd), session+".jsonl"), lines...)
}

func (w *world) prompt(session, cwd string, at time.Time, id string) {
	appendFile(w.t, filepath.Join(claudeDir(w.home, cwd), session+".jsonl"), mustJSON(map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": "please do the thing"}, "promptId": id,
		"timestamp": ts(at), "origin": map[string]any{"kind": "human"}, "cwd": cwd, "sessionId": session}))
}

func firstOr(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

// cx is a Codex token_count checkpoint: the thread's total and last call.
type cx struct{ totalIn, totalCached, totalOut, in, cached, out int64 }

func cxUsage(in, cached, out int64) map[string]any {
	return map[string]any{"input_tokens": in, "cached_input_tokens": cached, "output_tokens": out, "reasoning_output_tokens": 0, "total_tokens": in + out}
}

func (w *world) codexRollout(name, thread, cwd string, source any, at time.Time, counts []cx, zst bool) {
	day := filepath.Join(w.home, ".codex", "sessions", at.Format("2006"), at.Format("01"), at.Format("02"))
	lines := []string{
		mustJSON(map[string]any{"timestamp": ts(at), "type": "session_meta", "payload": map[string]any{"id": thread, "cwd": cwd, "source": source, "cli_version": "0.130.0"}}),
		mustJSON(map[string]any{"timestamp": ts(at), "type": "turn_context", "payload": map[string]any{"turn_id": "turn-1", "cwd": cwd, "model": "gpt-5.5"}}),
		mustJSON(map[string]any{"timestamp": ts(at.Add(time.Millisecond)), "type": "event_msg", "payload": map[string]any{"type": "user_message", "message": "fix it"}}),
		mustJSON(map[string]any{"timestamp": ts(at.Add(time.Millisecond)), "type": "response_item", "payload": map[string]any{"type": "message", "role": "assistant", "content": []any{}}}),
	}
	for i, c := range counts {
		lines = append(lines, mustJSON(map[string]any{"timestamp": ts(at.Add(time.Duration(2+i) * time.Millisecond)), "type": "event_msg",
			"payload": map[string]any{"type": "token_count", "info": map[string]any{
				"total_token_usage": cxUsage(c.totalIn, c.totalCached, c.totalOut),
				"last_token_usage":  cxUsage(c.in, c.cached, c.out)}}}))
	}
	path := filepath.Join(day, name+".jsonl")
	data := []byte(strings.Join(lines, "\n") + "\n")
	if zst {
		enc, _ := zstd.NewWriter(nil)
		data = enc.EncodeAll(data, nil)
		path += ".zst"
	}
	os.MkdirAll(day, 0o755)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		w.t.Fatal(err)
	}
}

// --- tests -------------------------------------------------------------------

// Two Claude Code calls, one written as three lines; Opus 5.5 at $4 input,
// $0.20 cache read, $5 5-minute and $8 1-hour cache write, $20 output.
//
//	A: 10 in, 200k read, 30k 1h write, 4k out = $0.36004
//	B:  5 in, 230k read,  5k 5m write, 1k out = $0.09102
func TestClaudeStandard(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	now := time.Now()
	w.prompt("S1", w.repo, now.Add(-10*time.Minute), "p1")
	w.assistant("S1", w.repo, now.Add(-9*time.Minute), cc{id: "msg_A", req: "req_A", in: 10, read: 200000, write1h: 30000, out: 4000}, 3)
	w.assistant("S1", w.repo, now.Add(-8*time.Minute), cc{id: "msg_B", req: "req_B", in: 5, read: 230000, write: 5000, out: 1000}, 1)
	w.assistant("S1", w.repo, now.Add(-7*time.Minute), cc{id: "msg_C", req: "req_C", model: "<synthetic>", out: 99}, 1)
	appendFile(t, filepath.Join(claudeDir(w.home, w.repo), "S1.jsonl"),
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"x"}]},"timestamp":"`+ts(now.Add(-8*time.Minute))+`","cwd":"`+w.repo+`","sessionId":"S1"}`,
		`not json at all`, `{"type":"assistant","message":`)
	// Another project's session never counts.
	other := filepath.Join(w.home, "work", "other")
	w.assistant("S9", other, now.Add(-5*time.Minute), cc{id: "msg_X", req: "req_X", in: 1, read: 1, out: 1000000}, 1)

	w.commit(nil, "-m", "feat: first")
	tr := w.trailers("HEAD")
	want := map[string]string{
		"AI-Cost":        "$0.451 (api-equiv)",
		"AI-Tokens":      "in=465k cached=430k written=35k out=5k",
		"AI-Model":       "claude-opus-5-5",
		"AI-Agent":       "claude-code",
		"AI-Turns":       "1",
		"AI-Cost-Schema": "1",
	}
	for k, v := range want {
		if got := strings.Join(tr[k], " | "); got != v {
			t.Errorf("%s = %q, want %q\n%s", k, got, v, w.message("HEAD"))
		}
	}
	// Settled usage is not billed again.
	msg := w.commit(nil, "-m", "chore: second")
	if strings.Contains(msg, "AI-Cost") {
		t.Errorf("second commit billed settled usage:\n%s", msg)
	}
	// New usage lands on the next commit.
	w.assistant("S1", w.repo, time.Now(), cc{id: "msg_D", req: "req_D", in: 1000, out: 1000}, 1)
	w.commit(nil, "-m", "feat: third")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.024 (api-equiv)" {
		t.Errorf("third commit AI-Cost = %v, want $0.024\n%s", got, w.message("HEAD"))
	}
}

// A commit an agent makes settles only that agent's session; the person's
// next commit picks up the rest.
func TestPreciseSession(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	now := time.Now()
	w.assistant("S1", w.repo, now.Add(-3*time.Minute), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1) // $0.06
	w.assistant("S2", w.repo, now.Add(-2*time.Minute), cc{id: "m2", req: "r2", in: 20000, out: 2000}, 1) // $0.12

	w.commit([]string{"CLAUDE_CODE_SESSION_ID=S1"}, "-m", "agent commit")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.060 (api-equiv)" {
		t.Fatalf("agent commit AI-Cost = %v, want $0.060\n%s", got, w.message("HEAD"))
	}
	w.commit(nil, "-m", "human commit")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.120 (api-equiv)" {
		t.Fatalf("human commit AI-Cost = %v, want $0.120 (S2 only)\n%s", got, w.message("HEAD"))
	}
	if msg := w.commit(nil, "-m", "nothing new"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("third commit billed again:\n%s", msg)
	}
}

// Codex: a compressed rollout with a repeated checkpoint, and a forked
// subagent replaying its parent's checkpoint with last = 0. gpt-5.5 at $5
// input, $0.50 cached, $30 output.
//
//	call 1: 10k in (8k cached), 600 out
//	call 2: 12k in (10k cached), 700 out
//	fork:   1k in, 100 out
func TestCodex(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	start := time.Now().Add(-20 * time.Minute)
	w.codexRollout("rollout-2026-10-26T10-00-00-T1", "T1", w.repo, "cli", start, []cx{
		{10000, 8000, 600, 10000, 8000, 600},
		{10000, 8000, 600, 10000, 8000, 600}, // re-emitted
		{22000, 18000, 1300, 12000, 10000, 700},
	}, true)
	w.codexRollout("rollout-2026-10-26T10-05-00-T2", "T2", w.repo,
		map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "T1"}}}, start.Add(5*time.Minute), []cx{
			{10000, 8000, 600, 0, 0, 0}, // parent's checkpoint, replayed
			{11000, 8000, 700, 1000, 0, 100},
		}, false)
	w.commit(nil, "-m", "codex work")
	tr := w.trailers("HEAD")
	if got := tr["AI-Cost"]; len(got) != 1 || got[0] != "$0.076 (api-equiv)" {
		t.Errorf("AI-Cost = %v, want $0.076\n%s", got, w.message("HEAD"))
	}
	if got := strings.Join(tr["AI-Tokens"], ""); got != "in=23k cached=18k out=1.4k" {
		t.Errorf("AI-Tokens = %q", got)
	}
	if got := strings.Join(tr["AI-Agent"], ""); got != "codex" {
		t.Errorf("AI-Agent = %q", got)
	}
	if got := strings.Join(tr["AI-Turns"], ""); got != "2" {
		t.Errorf("AI-Turns = %q, want 2", got)
	}
	// A commit the subagent makes, told by Codex's environment, settles its
	// own thread and leaves the parent's new call for the person.
	spawn := map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": "T1"}}}
	w.codexRollout("rollout-2026-10-26T10-30-00-T2", "T2", w.repo, spawn, time.Now(), []cx{{12000, 8000, 800, 1000, 0, 100}}, false)
	w.codexRollout("rollout-2026-10-26T10-30-01-T1", "T1", w.repo, "cli", time.Now(), []cx{{24000, 18000, 1400, 2000, 0, 100}}, false)
	w.commit([]string{"CODEX_THREAD_ID=T2"}, "-m", "subagent commit")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.008 (api-equiv)" {
		t.Errorf("subagent commit AI-Cost = %v, want $0.008 (its own call)\n%s", got, w.message("HEAD"))
	}
	w.commit(nil, "-m", "person's commit")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.013 (api-equiv)" {
		t.Errorf("person's commit AI-Cost = %v, want $0.013 (the parent's call)\n%s", got, w.message("HEAD"))
	}
}

// An amend carries the replaced commit's usage and the new usage, in one
// receipt.
func TestAmend(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	w.assistant("S1", w.repo, time.Now().Add(-time.Minute), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1) // $0.06
	w.commit(nil, "-m", "first")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m2", req: "r2", in: 20000, out: 2000}, 1) // $0.12
	msg := w.commit(nil, "--amend", "-m", "first, amended")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.180 (api-equiv)" {
		t.Fatalf("amend -m AI-Cost = %v, want $0.180\n%s", got, msg)
	}
	msg = w.commit(nil, "--amend", "--no-edit")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.180 (api-equiv)" {
		t.Fatalf("amend --no-edit AI-Cost = %v, want $0.180 once\n%s", got, msg)
	}
	if n := strings.Count(msg, "AI-Cost-Schema"); n != 1 {
		t.Errorf("%d receipts after two amends:\n%s", n, msg)
	}
	if msg := w.commit(nil, "-m", "next"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("usage settled by the amend billed again:\n%s", msg)
	}
}

// A message typed in the editor is stamped after the editor closes; an
// empty one still aborts the commit and settles nothing.
func TestEditor(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	editor := filepath.Join(w.home, "editor.sh")
	os.WriteFile(editor, []byte("#!/bin/sh\n{ echo 'feat: typed in the editor'; cat \"$1\"; } > \"$1.new\" && mv \"$1.new\" \"$1\"\n"), 0o755)
	w.assistant("S1", w.repo, time.Now().Add(-time.Minute), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)

	f := filepath.Join(w.repo, "file.txt")
	os.WriteFile(f, []byte("x\n"), 0o644)
	w.git("add", "-A")
	if out, err := w.run(w.repo, []string{"GIT_EDITOR=true"}, "git", "commit", "-q"); err == nil {
		t.Fatalf("an empty message was committed:\n%s", out)
	}
	out, err := w.run(w.repo, []string{"GIT_EDITOR=" + editor}, "git", "commit", "-q", "--verbose")
	if err != nil {
		t.Fatalf("editor commit: %v\n%s", err, out)
	}
	msg := w.message("HEAD")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.060 (api-equiv)" {
		t.Errorf("editor commit AI-Cost = %v, want $0.060\n%s", got, msg)
	}
	if strings.Contains(msg, "diff --git") || strings.Contains(msg, ">8") {
		t.Errorf("the --verbose diff leaked into the message:\n%s", msg)
	}
	if !strings.HasPrefix(msg, "feat: typed in the editor\n") {
		t.Errorf("subject moved:\n%s", msg)
	}
}

// A message that already ends in trailers keeps them all as trailers.
func TestExistingTrailers(t *testing.T) {
	w := newWorld(t)
	w.tab("install", "--template", "standard")
	w.assistant("S1", w.repo, time.Now().Add(-time.Minute), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "fix: thing\n\nWhy it broke.\n\nCo-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>\nSigned-off-by: Test <test@example.com>")
	tr := w.trailers("HEAD")
	for _, k := range []string{"Co-Authored-By", "Signed-off-by", "AI-Cost", "AI-Cost-Schema"} {
		if len(tr[k]) != 1 {
			t.Errorf("git sees %d %s trailers, want 1\n%s", len(tr[k]), k, w.message("HEAD"))
		}
	}
	// A one-line subject that looks like a trailer is still the title.
	w.assistant("S1", w.repo, time.Now(), cc{id: "m2", req: "r2", in: 10000, out: 1000}, 1)
	msg := w.commit(nil, "-m", "fix: subject only")
	if !strings.HasPrefix(msg, "fix: subject only\n\nAI-Cost:") {
		t.Errorf("trailers not in their own paragraph:\n%s", msg)
	}
}

// A merge's message is git's; its usage waits for the next commit.
func TestMerge(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	w.commit(nil, "-m", "base")
	w.git("checkout", "-q", "-b", "side")
	w.file = "side.txt"
	w.commit(nil, "-m", "side work")
	w.file = ""
	w.git("checkout", "-q", "main")
	w.commit(nil, "-m", "main work")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.git("merge", "-q", "--no-ff", "--no-edit", "side")
	if strings.Contains(w.message("HEAD"), "AI-Cost") {
		t.Errorf("merge commit stamped:\n%s", w.message("HEAD"))
	}
	w.commit(nil, "-m", "after merge")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.060 (api-equiv)" {
		t.Errorf("after-merge AI-Cost = %v, want $0.060", got)
	}
}

func TestTemplates(t *testing.T) {
	w := newWorld(t)
	w.tab("install", "--template", "minimal")
	w.prompt("S1", w.repo, time.Now().Add(-2*time.Minute), "p1")
	w.assistant("S1", w.repo, time.Now().Add(-time.Minute), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.assistant("S1", w.repo, time.Now().Add(-time.Minute), cc{id: "m2", req: "r2", model: "claude-haiku-5-5", in: 20000, out: 2000}, 1)
	w.commit(nil, "-m", "minimal")
	if got := strings.Join(w.trailers("HEAD")["AI-Cost"], ""); got != "$0.063 (api-equiv) · claude-opus-5-5 +1 · 1 turn" {
		t.Errorf("minimal = %q", got)
	}

	w.git("config", "tab.template", "fun")
	w.git("config", "tab.snacks", "coffees=0.03")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m3", req: "r3", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "fun")
	if got := strings.Join(w.trailers("HEAD")["AI-Cost-Equiv"], ""); got != "≈ 2 coffees" {
		t.Errorf("fun equiv = %q\n%s", got, w.message("HEAD"))
	}

	w.git("config", "tab.template", "receipt")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m4", req: "r4", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "receipt\n\nBody text.")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m5", req: "r5", in: 10000, out: 1000}, 1)
	msg := w.commit(nil, "--amend", "--no-edit")
	if n := strings.Count(msg, "A I   R E C E I P T"); n != 1 {
		t.Errorf("%d receipt drawings after amend:\n%s", n, msg)
	}
	if got := strings.Join(w.trailers("HEAD")["AI-Cost"], ""); got != "$0.120 (api-equiv)" {
		t.Errorf("receipt AI-Cost = %q\n%s", got, msg)
	}
	if !strings.Contains(msg, "Body text.\n\n ____") || !strings.Contains(msg, `\/\/\/`) {
		t.Errorf("receipt not drawn between body and trailers:\n%s", msg)
	}
	t.Logf("receipt commit:\n%s", msg)
}

// Nothing git-tab meets can stop a commit.
func TestNeverBlocks(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	// Unreadable and broken logs.
	dir := claudeDir(w.home, w.repo)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "S1.jsonl"), []byte("{\"type\":\"assistant\",\"message\":{\"usage\":{\"input_tokens\":\"many\"}}}\n\x00\x01\n"), 0o644)
	day := filepath.Join(w.home, ".codex", "sessions", "2026", "10", "26")
	os.MkdirAll(day, 0o755)
	os.WriteFile(filepath.Join(day, "rollout-x.jsonl.zst"), []byte("not zstd"), 0o644)
	locked := filepath.Join(w.home, ".claude", "projects", "-locked")
	os.MkdirAll(locked, 0o000)
	defer os.Chmod(locked, 0o755)
	if msg := w.commit(nil, "-m", "broken logs"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("stamped from garbage:\n%s", msg)
	}
	// An unknown model is counted, not priced.
	w.assistant("S1", w.repo, time.Now(), cc{id: "m1", req: "r1", model: "mystery-model-9", in: 1000, out: 10}, 1)
	w.commit(nil, "-m", "unknown model")
	if got := strings.Join(w.trailers("HEAD")["AI-Cost"], ""); got != "unpriced (api-equiv)" {
		t.Errorf("unpriced AI-Cost = %q", got)
	}
	// A hook timing out leaves the commit alone.
	w.git("config", "tab.timeout", "1ns")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m2", req: "r2", in: 1000, out: 10}, 1)
	w.commit(nil, "-m", "too slow")
	w.git("config", "--unset", "tab.timeout")
	// The binary gone: the hook does nothing.
	w.extra = []string{"PATH=/usr/bin:/bin:/opt/homebrew/bin:/usr/local/bin"}
	for _, ev := range []string{"prepare-commit-msg", "commit-msg", "post-commit"} {
		w.git("config", "hook.git-tab-"+ev+".command", `b='/nonexistent/git-tab'; [ -x "$b" ] || b=$(command -v git-tab) || exit 0; exec "$b" hook `+ev)
	}
	w.commit(nil, "-m", "no binary")
	w.extra = nil
}

// Hook files for git without config hooks: a hook already there keeps
// running, and comes back on uninstall.
func TestHookFiles(t *testing.T) {
	w := newWorld(t)
	hooks := filepath.Join(w.repo, ".git", "hooks")
	os.MkdirAll(hooks, 0o755)
	own := "#!/bin/sh\ngit interpret-trailers --in-place --trailer 'Issue: #42' \"$1\"\n"
	os.WriteFile(filepath.Join(hooks, "prepare-commit-msg"), []byte(own), 0o755)
	w.tab("install", "--files")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "with my own hook")
	tr := w.trailers("HEAD")
	if len(tr["Issue"]) != 1 || len(tr["AI-Cost"]) != 1 {
		t.Errorf("trailers %v\n%s", tr, w.message("HEAD"))
	}
	w.tab("uninstall")
	if b, _ := os.ReadFile(filepath.Join(hooks, "prepare-commit-msg")); string(b) != own {
		t.Errorf("own hook not restored:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(hooks, "post-commit")); err == nil {
		t.Errorf("post-commit hook left behind")
	}
}

func TestGlobalInstall(t *testing.T) {
	w := newWorld(t)
	if out, err := w.run(w.repo, nil, "git-tab", "install", "--global"); err != nil {
		// Config-defined hooks arrived in Git 2.54; an older git must refuse and say why.
		if !strings.Contains(out, "needs Git 2.54") {
			t.Fatalf("git tab install --global: %v\n%s", err, out)
		}
		t.Skip("this git predates config-defined hooks")
	}
	w.assistant("S1", w.repo, time.Now(), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "global")
	if len(w.trailers("HEAD")["AI-Cost"]) != 1 {
		t.Fatalf("not stamped by the global hooks:\n%s", w.message("HEAD"))
	}
	w.tab("uninstall", "--purge")
	if cfg, _ := os.ReadFile(filepath.Join(w.home, ".gitconfig")); strings.Contains(string(cfg), "git-tab") {
		t.Errorf("global config still has hooks:\n%s", cfg)
	}
	w.assistant("S1", w.repo, time.Now(), cc{id: "m2", req: "r2", in: 10000, out: 1000}, 1)
	if msg := w.commit(nil, "-m", "after uninstall"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("stamped after uninstall:\n%s", msg)
	}
}

// A session whose cwd is the symlinked form of the repository's path
// (macOS /var → /private/var) still counts.
func TestSymlinkedCwd(t *testing.T) {
	w := newWorld(t)
	if !strings.HasPrefix(w.repo, "/private/") {
		t.Skip("no /private symlinks here")
	}
	w.tab("install")
	logical := strings.TrimPrefix(w.repo, "/private")
	w.assistant("S1", logical, time.Now(), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "symlinked")
	if len(w.trailers("HEAD")["AI-Cost"]) != 1 {
		t.Errorf("not stamped:\n%s", w.message("HEAD"))
	}
}

// Work trees share a ledger. A subagent's work tree inside the repository
// (as under .claude/worktrees) bills only its own usage, and a commit in
// one work tree leaves the other's usage for the other's commits.
func TestWorktree(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	os.WriteFile(filepath.Join(w.repo, ".gitignore"), []byte(".claude/\n"), 0o644)
	w.commit(nil, "-m", "base")
	main, wt := w.repo, filepath.Join(w.repo, ".claude", "worktrees", "a")
	w.git("worktree", "add", "-q", "-b", "a", wt)
	w.assistant("S1", main, time.Now(), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1) // $0.06
	w.assistant("S2", wt, time.Now(), cc{id: "m2", req: "r2", in: 20000, out: 2000}, 1)   // $0.12

	w.repo = wt
	w.commit(nil, "-m", "in the linked work tree")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.120 (api-equiv)" {
		t.Fatalf("linked work tree AI-Cost = %v, want $0.120 (S2 only)\n%s", got, w.message("HEAD"))
	}
	if msg := w.commit(nil, "-m", "again"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("linked work tree billed twice:\n%s", msg)
	}
	w.repo = main
	w.commit(nil, "-m", "in the main work tree")
	if got := w.trailers("HEAD")["AI-Cost"]; len(got) != 1 || got[0] != "$0.060 (api-equiv)" {
		t.Fatalf("main work tree AI-Cost = %v, want $0.060 (S1 only)\n%s", got, w.message("HEAD"))
	}
	if msg := w.commit(nil, "-m", "nothing new"); strings.Contains(msg, "AI-Cost") {
		t.Errorf("main work tree billed twice:\n%s", msg)
	}
}

func TestReport(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "one")
	w.git("config", "tab.template", "minimal")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m2", req: "r2", in: 20000, out: 2000}, 1)
	w.commit(nil, "-m", "two")
	w.commit(nil, "-m", "three, by hand")
	sum := w.tab("summary")
	for _, want := range []string{"2 of 3 commits carry a receipt", "$0.18", "claude-opus-5-5", "$0.120"} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary lacks %q:\n%s", want, sum)
		}
	}
	if r := w.tab("receipt"); !strings.Contains(r, "TOTAL") || !strings.Contains(r, "$0.18") {
		t.Errorf("receipt:\n%s", r)
	}
	if b := w.tab("badge"); !strings.Contains(b, "https://img.shields.io/badge/built_with_%240.18_of_AI-8A2BE2") {
		t.Errorf("badge: %s", b)
	}
	if now := w.tab("now"); !strings.Contains(now, "no unsettled agent usage") {
		t.Errorf("now: %s", now)
	}
	t.Logf("summary:\n%s\nreceipt:\n%s", sum, w.tab("receipt"))
}
