package e2e

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var dollars = regexp.MustCompile(`\$(\d+(?:\.\d+)?)`)

// usd is the first dollar amount in s, or NaN.
func usd(s string) float64 {
	m := dollars.FindStringSubmatch(s)
	if m == nil {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(m[1], 64)
	return f
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// A Claude Max subscription at $125 a month whose month is worth $2,000 at
// API prices pays 6.25% of them; a DeepSeek key pays list price, also for
// the DeepSeek calls a gateway routes Claude Code to.
func TestPlan(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	w.tab("plan", "add", "claude-max", "125/mo", "claude-code", "--worth", "2000")
	w.tab("plan", "add", "deepseek", "api", "deepseek")
	now := time.Now()
	// $0.060 at Opus 5.5 prices; 100k DeepSeek V4 Pro input is $0.066 off-peak, $0.132 at peak.
	w.assistant("S1", w.repo, now.Add(-2*time.Minute), cc{id: "m1", req: "r1", in: 10000, out: 1000}, 1)
	w.assistant("S1", w.repo, now.Add(-time.Minute), cc{id: "m2", req: "r2", model: "deepseek-v4-pro", in: 100000}, 1)
	msg := w.commit(nil, "-m", "feat: on a plan")
	tr := w.trailers("HEAD")
	cost, paid := usd(strings.Join(tr["AI-Cost"], "")), usd(strings.Join(tr["AI-Cost-Paid"], ""))
	ds := cost - 0.060
	if !near(ds, 0.066, 0.002) && !near(ds, 0.132, 0.002) {
		t.Fatalf("AI-Cost = %v, want $0.060 of Opus and a DeepSeek part of $0.066 or $0.132\n%s", tr["AI-Cost"], msg)
	}
	if want := 0.060*0.0625 + ds; !near(paid, want, 0.002) {
		t.Errorf("AI-Cost-Paid = %v, want %.4f: Opus at 6.25%%, DeepSeek at list price\n%s", tr["AI-Cost-Paid"], want, msg)
	}
	for _, s := range []string{"SUBTOTAL", "PLAN DISCOUNT", "YOU PAID"} {
		if !strings.Contains(msg, s) {
			t.Errorf("the receipt has no %s line\n%s", s, msg)
		}
	}
	list := w.tab("plan")
	for _, s := range []string{"claude-max", "pays 6.25% of API prices", "deepseek"} {
		if !strings.Contains(list, s) {
			t.Errorf("git tab plan does not say %q\n%s", s, list)
		}
	}
	if sum := w.tab("summary"); !strings.Contains(sum, "you paid") {
		t.Errorf("the summary leaves out what was paid\n%s", sum)
	}
	// Without a plan the receipt keeps to API prices.
	w.tab("plan", "rm", "claude-max")
	w.assistant("S1", w.repo, time.Now(), cc{id: "m3", req: "r3", in: 10000, out: 1000}, 1)
	w.commit(nil, "-m", "feat: off the plan")
	if tr := w.trailers("HEAD"); len(tr["AI-Cost-Paid"]) != 0 {
		t.Errorf("a commit no plan pays for has AI-Cost-Paid %v", tr["AI-Cost-Paid"])
	}
}

// The receipt's running tab adds up every receipt in the history, an
// amend's included once.
func TestRunningTab(t *testing.T) {
	w := newWorld(t)
	w.tab("install")
	call := func(id string) {
		w.assistant("S1", w.repo, time.Now(), cc{id: id, req: "r" + id, in: 10000, out: 1000}, 1)
	}
	call("1")
	if msg := w.commit(nil, "-m", "one"); strings.Contains(msg, "TAB SO FAR") {
		t.Errorf("the first receipt has a running tab\n%s", msg)
	}
	call("2")
	w.commit(nil, "-m", "two")
	call("3")
	w.commit(nil, "--amend", "-m", "two, amended")
	call("4")
	msg := w.commit(nil, "-m", "three")
	line := ""
	for _, l := range strings.Split(msg, "\n") {
		if strings.Contains(l, "TAB SO FAR") {
			line = l
		}
	}
	var sum float64
	for _, l := range strings.Split(w.git("log", "--format=%(trailers:key=AI-Cost,valueonly)"), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			sum += usd(l)
		}
	}
	if !strings.Contains(line, "3 commits") || !near(usd(line), sum, 0.0005) {
		t.Errorf("running tab %q, want 3 commits and $%.3f\n%s", line, sum, msg)
	}
}

// Work moves between machines: this one commits, the session goes to a
// cloud box that commits too, and its log comes back here with the code.
// What the box's receipt covered is not billed again; what neither saw is.
func TestTwoMachines(t *testing.T) {
	cloud := newWorld(t)
	cloud.tab("install")
	cloud.commit(nil, "-m", "init")
	local := newWorld(t)
	local.file = "local.txt"
	local.git("pull", "-q", cloud.repo, "main")
	local.tab("install")
	local.assistant("S2", local.repo, time.Now(), cc{id: "a", req: "ra", in: 10000, out: 1000}, 1)
	local.commit(nil, "-m", "local work")
	if tr := local.trailers("HEAD"); len(tr["AI-Cost"]) != 1 {
		t.Fatalf("the local commit has no receipt")
	}

	// On the box: a call, committed with its receipt. Its author time is
	// in whole seconds, so it is set past the call.
	time.Sleep(10 * time.Millisecond)
	t1 := time.Now()
	cloud.assistant("S1", cloud.repo, t1, cc{id: "b", req: "rb", in: 10000, out: 1000}, 1)
	authored := t1.Truncate(time.Second).Add(time.Second)
	cloud.commit([]string{"GIT_AUTHOR_DATE=@" + strconv.FormatInt(authored.Unix(), 10) + " +0000"}, "-m", "cloud work")
	if tr := cloud.trailers("HEAD"); len(tr["AI-Session"]) != 1 || tr["AI-Session"][0] != "claude-code/S1" {
		t.Fatalf("the box's receipt names sessions %v, want claude-code/S1", tr["AI-Session"])
	}

	// Back here: the code is pulled and the session's log copied, with
	// this machine's paths; then more work in both sessions.
	local.git("pull", "-q", "--no-rebase", "--no-edit", cloud.repo, "main")
	local.assistant("S1", local.repo, t1, cc{id: "b", req: "rb", in: 10000, out: 1000}, 1)
	time.Sleep(time.Until(authored.Add(20 * time.Millisecond)))
	local.assistant("S1", local.repo, time.Now(), cc{id: "c", req: "rc", in: 20000}, 1)
	local.assistant("S2", local.repo, time.Now(), cc{id: "d", req: "rd", in: 5000}, 1)
	msg := local.commit(nil, "-m", "back home")
	if got := usd(strings.Join(local.trailers("HEAD")["AI-Cost"], "")); !near(got, 0.100, 0.0005) {
		t.Errorf("AI-Cost = $%v, want $0.100: the box's $0.060 call is on its own receipt\n%s", got, msg)
	}
	// The running tab counts the box's receipt: it came with the code.
	if !strings.Contains(msg, "TAB SO FAR · 3 commits") || !strings.Contains(msg, "$0.220") {
		t.Errorf("the running tab misses the box's receipt\n%s\n%s", msg, local.git("log", "--graph", "--format=%h %s %(trailers:key=AI-Cost)"))
	}
	if _, err := os.Stat(filepath.Join(local.repo, ".git", "git-tab", "ledger.json")); err != nil {
		t.Errorf("no ledger here: %v", err)
	}
}

// `git tab agents` leaves word for agents elsewhere; on a Claude Code
// cloud box the hook it adds installs git-tab, and GIT_TAB_PLANS carries
// the plans there.
func TestCloud(t *testing.T) {
	w := newWorld(t)
	os.WriteFile(filepath.Join(w.repo, "CLAUDE.md"), []byte("# Rules\n- be nice"), 0o644)
	os.MkdirAll(filepath.Join(w.repo, ".claude"), 0o755)
	os.WriteFile(filepath.Join(w.repo, ".claude", "settings.json"), []byte(`{"model": "claude-opus-5-5", "hooks": {"Stop": []}}`), 0o644)
	w.tab("agents")
	again := w.tab("agents")
	// Claude Code reads the CLAUDE.md; no AGENTS.md is made beside it.
	b, _ := os.ReadFile(filepath.Join(w.repo, "CLAUDE.md"))
	if n := strings.Count(string(b), "AI cost receipts"); n != 1 || !strings.HasPrefix(string(b), "# Rules\n- be nice\n") {
		t.Errorf("CLAUDE.md has the line %d times, want once after the rules\n%s\n%s", n, b, again)
	}
	if _, err := os.Stat(filepath.Join(w.repo, "AGENTS.md")); err == nil {
		t.Errorf("an AGENTS.md was made beside the CLAUDE.md")
	}
	b, _ = os.ReadFile(filepath.Join(w.repo, ".claude", "settings.json"))
	var settings struct {
		Model string
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(b, &settings); err != nil || settings.Model != "claude-opus-5-5" || len(settings.Hooks["SessionStart"]) != 1 {
		t.Fatalf("settings.json lost a key or has no SessionStart hook: %v\n%s", err, b)
	}
	hook := settings.Hooks["SessionStart"][0].Hooks[0].Command
	w.commit(nil, "-m", "init")

	// The box: a fresh clone with git-tab's binary in place and no hooks.
	box := newWorld(t)
	box.git("pull", "-q", w.repo, "main")
	os.MkdirAll(filepath.Join(box.home, ".local", "bin"), 0o755)
	exe, _ := os.ReadFile(filepath.Join(binDir, "git-tab"))
	os.WriteFile(filepath.Join(box.home, ".local", "bin", "git-tab"), exe, 0o755)
	if out, _ := box.run(box.repo, []string{"CLAUDE_PROJECT_DIR=" + box.repo}, "sh", "-c", hook); strings.Contains(box.tab("doctor"), "post-commit:") {
		t.Fatalf("the hook installed git-tab outside the cloud\n%s", out)
	}
	if out, err := box.run(box.repo, []string{"CLAUDE_CODE_REMOTE=true", "CLAUDE_PROJECT_DIR=" + box.repo}, "sh", "-c", hook); err != nil {
		t.Fatalf("cloud hook: %v\n%s", err, out)
	}
	if d := box.tab("doctor"); !strings.Contains(d, "post-commit:") {
		t.Fatalf("the cloud hook did not install the hooks\n%s", d)
	}
	box.assistant("S9", box.repo, time.Now(), cc{id: "m", req: "r", in: 10000, out: 1000}, 1)
	box.commit([]string{"GIT_TAB_PLANS=claude-max=125/mo@2000:claude-code"}, "-m", "from the cloud")
	if paid := box.trailers("HEAD")["AI-Cost-Paid"]; len(paid) != 1 || !strings.HasPrefix(paid[0], "$0.0037") && !strings.HasPrefix(paid[0], "$0.0038") {
		t.Errorf("AI-Cost-Paid = %v, want $0.06 at 6.25%%", paid)
	}
}
