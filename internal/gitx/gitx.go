// Package gitx runs the few git commands git-tab needs.
package gitx

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Run runs git in dir and returns its output without the final newline.
func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && stderr.Len() > 0 {
		err = errors.New(strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(string(out), "\n"), err
}

type Repo struct {
	Top       string // the work tree's root, symlinks resolved
	GitDir    string // this work tree's git directory
	CommonDir string // the repository's, shared by its work trees
	Head      string // "" before the first commit
}

func Open(dir string) (*Repo, error) {
	// One git call: before the first commit the HEAD lookup fails, but only
	// after the three paths are printed.
	out, err := Run(dir, "rev-parse", "--show-toplevel", "--absolute-git-dir", "--git-common-dir", "--verify", "-q", "HEAD^{commit}")
	f := strings.Split(out, "\n")
	if len(f) < 3 || f[0] == "" {
		if err == nil {
			err = errors.New("not in a work tree")
		}
		return nil, err
	}
	r := &Repo{Top: f[0], GitDir: f[1], CommonDir: f[2]}
	if err == nil && len(f) > 3 {
		r.Head = f[3]
	}
	if !filepath.IsAbs(r.CommonDir) {
		base := dir
		if base == "" {
			base, _ = os.Getwd()
		}
		r.CommonDir = filepath.Join(base, r.CommonDir)
	}
	if t, err := filepath.EvalSymlinks(r.Top); err == nil {
		r.Top = t
	}
	return r, nil
}

// Linked says whether the repository has more than one work tree.
func (r *Repo) Linked() bool {
	a, errA := os.Stat(r.GitDir)
	b, errB := os.Stat(r.CommonDir)
	if errA == nil && errB == nil && !os.SameFile(a, b) {
		return true
	}
	entries, _ := os.ReadDir(filepath.Join(r.CommonDir, "worktrees"))
	return len(entries) > 0
}

// Config is every tab.* setting (lower-cased keys) and core.commentChar.
func (r *Repo) Config() map[string]string {
	out, _ := Run(r.Top, "config", "--get-regexp", `^(tab\.|tab-plan\.|core\.commentchar$)`)
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, " "); ok {
			m[strings.ToLower(k)] = v
		} else if l != "" {
			m[strings.ToLower(l)] = "true"
		}
	}
	return m
}

// CommitTime is a commit's committer time.
func (r *Repo) CommitTime(rev string) (time.Time, error) {
	out, err := Run(r.Top, "log", "-1", "--format=%ct", rev, "--")
	if err != nil {
		return time.Time{}, err
	}
	s, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(s, 0), nil
}

// Parents are a commit's parents.
func (r *Repo) Parents(rev string) []string {
	out, err := Run(r.Top, "rev-list", "--parents", "-n", "1", rev, "--")
	if err != nil {
		return nil
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return nil
	}
	return f[1:]
}

// InSequence says which history-rewriting operation is replaying commits
// in this work tree: "rebase", "cherry-pick", "revert" or "".
func (r *Repo) InSequence() string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(r.GitDir, name))
		return err == nil
	}
	switch {
	case exists("rebase-merge"), exists("rebase-apply"):
		return "rebase"
	case exists("CHERRY_PICK_HEAD"):
		return "cherry-pick"
	case exists("REVERT_HEAD"):
		return "revert"
	}
	return ""
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// Version is the installed git's major and minor version.
func Version() (int, int) {
	out, err := Run("", "version")
	if err != nil {
		return 0, 0
	}
	m := versionRe.FindStringSubmatch(out)
	if m == nil {
		return 0, 0
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return a, b
}

// ConfigHooks says whether git runs hooks defined in its config
// (hook.<name>.command), which Git 2.54 introduced.
func ConfigHooks() bool {
	a, b := Version()
	return a > 2 || a == 2 && b >= 54
}
