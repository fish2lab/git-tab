// Package stamp is what the git hooks do: find the usage a commit is to
// settle, put its receipt in the message, and record the settlement once
// the commit exists.
//
//	prepare-commit-msg  a message given with -m, -F, -c, -C or --amend is stamped here
//	commit-msg          a message typed in the editor is stamped here, after it closes
//	post-commit         the commit exists: its usage is settled in the ledger
//
// Nothing here may stop a commit: every failure leaves the message as it
// was, and the hooks always exit 0.
package stamp

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fish2lab/git-tab/internal/agents"
	"github.com/fish2lab/git-tab/internal/bill"
	"github.com/fish2lab/git-tab/internal/gitx"
	"github.com/fish2lab/git-tab/internal/ledger"
	"github.com/fish2lab/git-tab/internal/price"
	"github.com/fish2lab/git-tab/internal/render"
)

// Settings are a repository's tab.* config.
type Settings struct {
	Enabled     bool
	Template    string
	Snacks      []render.Snack
	Timeout     time.Duration
	Lookback    time.Duration
	RepoScope   bool // ignore the committing agent's session; settle everyone's
	AutoPrices  bool
	CommentChar string
}

func settings(cfg map[string]string) Settings {
	s := Settings{Enabled: true, Template: render.Standard, Timeout: 4 * time.Second, Lookback: 30 * 24 * time.Hour,
		AutoPrices: true, CommentChar: "#"}
	if v, ok := cfg["tab.enabled"]; ok && isFalse(v) {
		s.Enabled = false
	}
	if os.Getenv("GIT_TAB_DISABLE") != "" {
		s.Enabled = false
	}
	if v := strings.ToLower(cfg["tab.template"]); slices.Contains(render.Templates, v) {
		s.Template = v
	}
	snacks := render.DefaultSnacks
	if v, ok := cfg["tab.snacks"]; ok {
		snacks = v
	}
	s.Snacks = render.ParseSnacks(snacks)
	if d, err := time.ParseDuration(cfg["tab.timeout"]); err == nil && d > 0 {
		s.Timeout = d
	}
	if d, err := parseDays(cfg["tab.lookback"]); err == nil && d > 0 {
		s.Lookback = d
	}
	s.RepoScope = strings.ToLower(cfg["tab.scope"]) == "repo"
	if v, ok := cfg["tab.autoupdateprices"]; ok && isFalse(v) {
		s.AutoPrices = false
	}
	// "auto" picks a character the message does not use; git-tab writes no
	// line starting with one, so only "#" and explicit ones matter.
	if v := cfg["core.commentchar"]; v != "" && v != "auto" {
		s.CommentChar = v
	}
	return s
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "no", "off", "0":
		return true
	}
	return false
}

// parseDays reads a duration that may be given in days: "30d", "720h".
func parseDays(v string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(strings.TrimSpace(v), "d"); ok {
		d, err := strconv.Atoi(n)
		return time.Duration(d) * 24 * time.Hour, err
	}
	return time.ParseDuration(v)
}

// Scope is the agent session a commit comes from, when the commit was made
// by an agent that says so in its environment. Its ids name the session or
// one of its threads.
type Scope struct {
	Agent string
	IDs   []string
}

func (s *Scope) Matches(e agents.Event) bool {
	if s == nil {
		return true
	}
	return e.Agent == s.Agent && (slices.Contains(s.IDs, e.Session) || slices.Contains(s.IDs, e.Thread))
}

func (s *Scope) String() string {
	if s == nil {
		return "repo"
	}
	return s.Agent + ":" + strings.Join(s.IDs, ",")
}

// EnvScope reads the committing agent's session from the environment:
// GIT_TAB_SESSION=<agent>:<id> for any agent, else what Claude Code
// (CLAUDE_CODE_SESSION_ID) and Codex (CODEX_THREAD_ID, CODEX_SESSION_ID)
// pass to the commands they run.
func EnvScope() *Scope {
	if v := os.Getenv("GIT_TAB_SESSION"); v != "" {
		if a, id, ok := strings.Cut(v, ":"); ok && id != "" {
			return &Scope{Agent: a, IDs: []string{id}}
		}
	}
	if v := os.Getenv("CLAUDE_CODE_SESSION_ID"); v != "" {
		return &Scope{Agent: "claude-code", IDs: []string{v}}
	}
	var ids []string
	for _, k := range []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		if v := os.Getenv(k); v != "" {
			ids = append(ids, v)
		}
	}
	if len(ids) > 0 {
		return &Scope{Agent: "codex", IDs: ids}
	}
	return nil
}

// Window is the usage a commit can settle.
type Window struct {
	State ledger.State
	Floor time.Time // nothing older counts, whatever the ledger says
	End   time.Time
	Scope *Scope
}

// key names what the ledger settles separately: a thread, so that a
// subagent's commit leaves its parent's other calls for later.
func key(e agents.Event) string { return e.Agent + ":" + e.Thread }

// Collect is the window's events whose cwd is in the work tree at top, and
// the threads they belong to.
func Collect(top string, w Window) ([]agents.Event, []string, []error) {
	since := w.State.Earliest()
	if since.Before(w.Floor) {
		since = w.Floor
	}
	cwds := newResolver(top, w.Scope != nil)
	// git names the work tree by its real path; an agent may have logged
	// the symlinked one (macOS /tmp and /var live under /private).
	dirs := []string{top}
	if rest, ok := strings.CutPrefix(top, "/private/"); ok {
		dirs = append(dirs, "/"+rest)
	}
	var events []agents.Event
	threads := map[string]bool{}
	errs := agents.Scan(agents.Query{Since: since, Until: w.End, Dirs: dirs}, func(e agents.Event) {
		if !w.Scope.Matches(e) {
			return
		}
		start := w.State.Start(key(e))
		if start.Before(w.Floor) {
			start = w.Floor
		}
		if !e.Time.After(start) || e.Time.After(w.End) || !cwds.match(e.Cwd) {
			return
		}
		events = append(events, e)
		threads[key(e)] = true
	})
	var keys []string
	for k := range threads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return events, keys, errs
}

// resolver says whether a session's cwd is in a work tree: at or below its
// root, and not inside a nested repository or work tree of its own. With
// above set, a cwd above the root counts too: a session that an agent
// started higher up and that made this very commit.
type resolver struct {
	top   string
	above bool
	seen  map[string]bool
}

func newResolver(top string, above bool) *resolver {
	if t, err := filepath.EvalSymlinks(top); err == nil {
		top = t
	}
	return &resolver{top: filepath.Clean(top), above: above, seen: map[string]bool{}}
}

func (r *resolver) match(cwd string) bool {
	if cwd == "" {
		return false
	}
	if v, ok := r.seen[cwd]; ok {
		return v
	}
	c := filepath.Clean(cwd)
	if t, err := filepath.EvalSymlinks(c); err == nil {
		c = t
	}
	v := false
	switch {
	case c == r.top:
		v = true
	case strings.HasPrefix(c, r.top+string(filepath.Separator)):
		v = true
		for d := c; d != r.top && len(d) > len(r.top); d = filepath.Dir(d) {
			if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
				v = false
				break
			}
		}
	case r.above && strings.HasPrefix(r.top, c+string(filepath.Separator)):
		v = true
	}
	r.seen[cwd] = v
	return v
}

// hook is one hook's run: the repository, its settings and what to log.
type hook struct {
	repo  *gitx.Repo
	s     Settings
	log   []string
	start time.Time
}

// opened is the repository this process runs in and its settings, looked
// up once: each git call costs a hook several milliseconds.
var opened struct {
	sync.Once
	repo *gitx.Repo
	s    Settings
	err  error
}

var began = time.Now()

func open() (*hook, error) {
	opened.Do(func() {
		if opened.repo, opened.err = gitx.Open(""); opened.err == nil {
			opened.s = settings(opened.repo.Config())
		}
	})
	if opened.err != nil {
		return nil, opened.err
	}
	return &hook{repo: opened.repo, s: opened.s, start: began}, nil
}

// Idle says, without running git, that a hook has nothing to do:
// commit-msg and post-commit only act on what prepare-commit-msg left.
func Idle(event string, args []string) bool {
	var acts func(*ledger.Pending) bool
	switch event {
	case "commit-msg":
		acts = func(p *ledger.Pending) bool { return p.Deferred && !p.Stamped }
	case "post-commit":
		acts = func(p *ledger.Pending) bool { return p.Stamped }
	default:
		return false
	}
	dir := gitDir(args)
	if dir == "" {
		return false
	}
	p, ok := ledger.ReadPending(dir)
	if ok && acts(p) {
		return false
	}
	if event == "post-commit" {
		ledger.ClearPending(dir)
	}
	return true
}

// gitDir is a hook's git directory when it can be told without running
// git: the message file commit-msg gets is in it, git sets GIT_DIR for a
// linked work tree, and hooks run at the top of the work tree. It is ""
// when none of these leads to a git directory.
func gitDir(args []string) string {
	var dirs []string
	if len(args) > 0 {
		dirs = append(dirs, filepath.Dir(args[0]))
	}
	if d := os.Getenv("GIT_DIR"); d != "" {
		dirs = append(dirs, d)
	}
	for _, d := range append(dirs, ".git") {
		if info, err := os.Stat(filepath.Join(d, "HEAD")); err == nil && info.Mode().IsRegular() {
			if abs, err := filepath.Abs(d); err == nil {
				return abs
			}
		}
	}
	return ""
}

func (h *hook) logf(format string, a ...any) { h.log = append(h.log, fmt.Sprintf(format, a...)) }

// flush keeps the run's notes for `git tab doctor`.
func (h *hook) flush(event string) {
	if h == nil || h.repo == nil {
		return
	}
	head := fmt.Sprintf("%s %s (%s)", time.Now().Format(time.RFC3339), event, time.Since(h.start).Round(time.Millisecond))
	body := strings.Join(append([]string{head}, h.log...), "\n  ") + "\n"
	ledger.WriteFile(filepath.Join(h.repo.GitDir, "git-tab", "last-"+event+".log"), []byte(body))
}

// Guard runs a hook so that nothing it does can stop the commit: a panic
// is swallowed and a hook still busy at the deadline is left behind.
func Guard(timeout time.Duration, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { recover() }()
		fn()
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Timeout is the deadline a repository sets its hooks, read without
// waiting for anything else.
func Timeout() time.Duration {
	h, err := open()
	if err != nil {
		return 4 * time.Second
	}
	return h.s.Timeout
}

// Prepare is the prepare-commit-msg hook: args are the message file, the
// message's source and, for a "commit" source, the commit it came from.
func Prepare(args []string) {
	if len(args) == 0 {
		return
	}
	h, err := open()
	if err != nil {
		return
	}
	defer h.flush("prepare-commit-msg")
	ledger.ClearPending(h.repo.GitDir)
	if !h.s.Enabled {
		h.logf("disabled")
		return
	}
	file, source, sha := args[0], at(args, 1), at(args, 2)
	h.logf("source=%q sha=%s head=%s", source, short(sha), short(h.repo.Head))
	raw, err := os.ReadFile(file)
	if err != nil {
		h.logf("read message: %v", err)
		return
	}
	msg := string(raw)
	cc := h.s.CommentChar
	switch seq := h.repo.InSequence(); seq {
	case "rebase":
		h.logf("rebase: the replayed message keeps its receipt")
		return
	case "cherry-pick", "revert":
		// The inherited receipt is the original commit's work, not this one's.
		if s := render.Strip(msg, cc); s != msg {
			ledger.WriteFile(file, []byte(s))
		}
		h.logf("%s: inherited receipt removed", seq)
		return
	}
	if source == "merge" || source == "squash" {
		h.logf("%s: git wrote this message; its usage waits for the next commit", source)
		return
	}
	p := &ledger.Pending{Head: h.repo.Head, Source: source}
	// git gives an amend the source "commit", or "message" with -m or -F.
	if h.repo.Head != "" && (source == "commit" || source == "message") {
		amend, known := amendFlag()
		p.Amend = amend || !known && source == "commit" && sha == h.repo.Head
	}
	stripped := render.Strip(msg, cc)
	if source == "template" || !render.HasContent(stripped, cc) {
		// The editor is about to open on an empty message. Stamping it now
		// would keep git from aborting when it is left empty.
		if stripped != msg {
			ledger.WriteFile(file, []byte(stripped))
		}
		p.Deferred = true
		ledger.WritePending(h.repo.GitDir, p)
		h.logf("deferred to commit-msg (amend=%v)", p.Amend)
		return
	}
	out, ok := h.stamp(p, stripped)
	if !ok {
		if stripped != msg {
			ledger.WriteFile(file, []byte(stripped))
		}
		return
	}
	if err := ledger.WriteFile(file, []byte(out)); err != nil {
		h.logf("write message: %v", err)
		return
	}
	ledger.WritePending(h.repo.GitDir, p)
}

// CommitMsg is the commit-msg hook: it stamps a message typed in the editor.
func CommitMsg(args []string) {
	if len(args) == 0 {
		return
	}
	h, err := open()
	if err != nil {
		return
	}
	defer h.flush("commit-msg")
	p, ok := ledger.ReadPending(h.repo.GitDir)
	if !ok || !p.Deferred || p.Stamped || !h.s.Enabled {
		return
	}
	raw, err := os.ReadFile(args[0])
	if err != nil {
		return
	}
	msg := string(raw)
	if !render.HasContent(msg, h.s.CommentChar) {
		h.logf("empty message: git will abort")
		return
	}
	out, ok := h.stamp(p, render.Strip(msg, h.s.CommentChar))
	if !ok {
		return
	}
	if err := ledger.WriteFile(args[0], []byte(out)); err != nil {
		h.logf("write message: %v", err)
		return
	}
	ledger.WritePending(h.repo.GitDir, p)
}

// stamp puts the receipt of the usage p's commit settles in msg.
func (h *hook) stamp(p *ledger.Pending, msg string) (string, bool) {
	b, w, threads, err := h.bill(p.Amend, EnvScope())
	if err != nil {
		h.logf("%v", err)
		return msg, false
	}
	if b.Empty() {
		h.logf("no agent usage since %s", w.State.Earliest().Format(time.RFC3339))
		return msg, false
	}
	art, trailers := render.Render(b, h.s.Template, h.s.Snacks)
	p.Before, p.End, p.Cost, p.Stamped = w.State, w.End, b.Cost, true
	// Work trees share the ledger, and a commit in one sees none of the
	// usage in the others: it settles only the threads it billed.
	if w.Scope != nil || h.repo.Linked() {
		p.Threads = threads
	}
	h.logf("stamped %s: calls=%d turns=%d template=%s scope=%s", render.USD(b.Cost), b.Requests, b.Turns, h.s.Template, w.Scope)
	return render.Apply(msg, art, trailers, h.s.CommentChar), true
}

// bill is the usage the next commit settles. amend says it replaces HEAD,
// whose settlement it then takes over.
func (h *hook) bill(amend bool, scope *Scope) (*bill.Bill, Window, []string, error) {
	l, err := ledger.Open(h.repo.CommonDir)
	if err != nil {
		return nil, Window{}, nil, err
	}
	now := time.Now()
	w := Window{State: l.State, End: now, Floor: now.Add(-h.s.Lookback)}
	if h.s.RepoScope {
		scope = nil
	}
	w.Scope = scope
	if c, ok := l.Commits[h.repo.Head]; amend && ok && l.Last == h.repo.Head {
		w.State = c.Before
	} else if l.Fresh() {
		// First run in this repository: what came before the commit being
		// built on (or, for an amend, before the one being replaced) is its.
		rev := "HEAD"
		if amend {
			rev = "HEAD^"
		}
		if t, err := h.repo.CommitTime(rev); err == nil {
			w.State.Base = t
		}
	}
	events, threads, errs := Collect(h.repo.Top, w)
	for _, e := range errs {
		h.logf("%v", e)
	}
	cat := price.Load()
	h.logf("%d events, prices %s %s", len(events), cat.From, cat.Fetched.Format("2006-01-02"))
	return bill.Build(events, cat, now), w, threads, nil
}

// PostCommit is the post-commit hook: the commit exists, so the usage on
// its receipt is settled.
func PostCommit() {
	h, err := open()
	if err != nil {
		return
	}
	defer h.flush("post-commit")
	p, ok := ledger.ReadPending(h.repo.GitDir)
	ledger.ClearPending(h.repo.GitDir)
	if !ok || !p.Stamped || h.repo.Head == "" || h.repo.Head == p.Head {
		return
	}
	// The truth of an amend: the new commit does not sit on the old HEAD.
	amend := p.Head != "" && !slices.Contains(h.repo.Parents(h.repo.Head), p.Head)
	unlock := ledger.Lock(h.repo.CommonDir)
	defer unlock()
	l, err := ledger.Open(h.repo.CommonDir)
	if err != nil {
		h.logf("%v", err)
		return
	}
	state := l.State
	if l.Fresh() {
		state = p.Before
	}
	if c, ok := l.Commits[p.Head]; amend && p.Amend && ok && l.Last == p.Head {
		state = c.Before
	}
	l.State = state.Settle(p.End, p.Threads).Prune(time.Now().Add(-h.s.Lookback))
	if amend {
		delete(l.Commits, p.Head)
	}
	l.Commits[h.repo.Head] = ledger.Commit{Before: p.Before, End: p.End, Threads: p.Threads, Cost: p.Cost}
	l.Last = h.repo.Head
	if err := l.Save(); err != nil {
		h.logf("save ledger: %v", err)
	}
	h.logf("settled %s up to %s (amend=%v)", short(h.repo.Head), p.End.Format(time.RFC3339), amend)
	if h.s.AutoPrices {
		refreshPrices()
	}
}

// refreshPrices updates a week-old price cache in a detached process, at
// most once an hour, so the hook never waits on the network.
func refreshPrices() {
	if !price.Stale(7 * 24 * time.Hour) {
		return
	}
	mark := price.CachePath() + ".attempt"
	if info, err := os.Stat(mark); err == nil && time.Since(info.ModTime()) < time.Hour {
		return
	}
	os.MkdirAll(filepath.Dir(mark), 0o755)
	os.WriteFile(mark, nil, 0o644)
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "prices", "update", "--quiet")
	detach(cmd)
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// amendFlag looks for --amend in the git command that runs this hook: git
// tells prepare-commit-msg the source "message" for `commit --amend -m`.
// known is false when the command could not be found.
func amendFlag() (amend, known bool) {
	pid := os.Getppid()
	for i := 0; i < 5 && pid > 1; i++ {
		out, err := exec.Command("ps", "-o", "ppid=", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return false, false
		}
		f := strings.Fields(string(out))
		if len(f) < 2 {
			return false, false
		}
		if base := filepath.Base(f[1]); base == "git" || base == "git.exe" {
			for _, a := range f[2:] {
				if a == "--amend" {
					return true, true
				}
			}
			if slices.Contains(f[2:], "commit") {
				return false, true
			}
		}
		pid, _ = strconv.Atoi(f[0])
	}
	return false, false
}

// Preview is the bill the next commit would carry, for `git tab now`.
type Preview struct {
	Bill     *bill.Bill
	Window   Window
	Art      []string
	Trailers []string
	Settings Settings
	Repo     *gitx.Repo
}

func Next(repoScope bool) (*Preview, error) {
	h, err := open()
	if err != nil {
		return nil, errors.New("not in a git work tree")
	}
	if repoScope {
		h.s.RepoScope = true
	}
	b, w, _, err := h.bill(false, EnvScope())
	if err != nil {
		return nil, err
	}
	pv := &Preview{Bill: b, Window: w, Settings: h.s, Repo: h.repo}
	if !b.Empty() {
		pv.Art, pv.Trailers = render.Render(b, h.s.Template, h.s.Snacks)
	}
	return pv, nil
}

func at(args []string, i int) string {
	if i < len(args) {
		return args[i]
	}
	return ""
}

func short(sha string) string {
	if len(sha) > 9 {
		return sha[:9]
	}
	return sha
}
