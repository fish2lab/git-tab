package agents

import (
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// OpenCode keeps sessions as rows, not lines. OpenCode 2 has them in its
// SQLite database (opencode.db) in session_v2 (directory, parent_id) and
// session_message, a row a message with its kind in type (user, assistant,
// compaction, synthetic, idle, ...), its order in seq and the rest as JSON
// in data. An assistant's data has model {id, providerID}, tokens {input
// without the cache, output without the reasoning, reasoning, cache {read,
// write}}, cost and time {created, completed}. A user row is a prompt.
// Versions 1.2 to 2 kept the same documents in session, message and part,
// which OpenCode 2 copies into its own tables once, recording kv
// migration.v1-v2 {"phase":"completed"} when done; older versions kept
// them as JSON files under storage/ (session/<project>/<id>.json,
// message/<session>/<id>.json), which stay behind once there is a database.
// A subagent's work is a session whose parent_id is the session it ran in,
// and bills there.
//
// ZCode's CLI, OpenCode made over, keeps the 1.2 tables in a database of its
// own (~/.zcode/cli/db/db.sqlite), with the AI SDK's token counts: input
// with the cache in it, output with the reasoning. Its prompts are user
// messages from a real_user.
//
// Following magpie: where there is a database only it is read, and of it
// only OpenCode 2's tables once the copy is done; while it is under way the
// sessions not yet copied are read from the old ones. The database is
// opened read-only and never written.

func init() {
	Register(openCode{name: "opencode"})
	Register(openCode{name: "zcode"})
}

type openCode struct{ name string }

func (o openCode) Name() string { return o.name }

func openCodeDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "opencode")
	}
	return filepath.Join(home(), ".local", "share", "opencode")
}

// db is the database: $OPENCODE_DB (in the data folder unless absolute),
// else opencode.db there; ZCode's in its own folder.
func (o openCode) db() string {
	if o.name == "zcode" {
		return filepath.Join(home(), ".zcode", "cli", "db", "db.sqlite")
	}
	if p := os.Getenv("OPENCODE_DB"); p != "" && p != ":memory:" {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(openCodeDir(), p)
	}
	return filepath.Join(openCodeDir(), "opencode.db")
}

func (o openCode) Roots() []string {
	dir := filepath.Dir(o.db())
	if o.name == "opencode" && dir != openCodeDir() {
		return []string{dir, openCodeDir()}
	}
	return []string{dir}
}

// ocMessage is a message's JSON document, in any of OpenCode's stores.
type ocMessage struct {
	Role string `json:"role"` // OpenCode 2 has it in the row's type
	// ZCode's newer messages say modelId, which the decoder matches too
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Model      json.RawMessage `json:"model"` // OpenCode 2's {id, providerID}
	Cost       float64         `json:"cost"`
	Time       struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Path *struct {
		Cwd string `json:"cwd"`
	} `json:"path"`
	Tokens *struct {
		Input     int64 `json:"input"`
		Output    int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache     struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Semantics *struct {
		Origin string `json:"origin"`
	} `json:"semantics"` // ZCode's: who a message is from
}

func (m *ocMessage) model() (provider, model string) {
	var v struct {
		ID         string `json:"id"`
		ProviderID string `json:"providerID"`
	}
	if len(m.Model) > 0 && json.Unmarshal(m.Model, &v) == nil && v.ID != "" {
		return v.ProviderID, v.ID
	}
	return m.ProviderID, m.ModelID
}

type ocSession struct{ parent, dir string }

// ocSessions are sessions by id, to find the one each ran under.
type ocSessions map[string]ocSession

// root is the session at the top of id's: a subagent's subagent bills there
// too.
func (s ocSessions) root(id string) string {
	for n := 0; s[id].parent != "" && n < 64; n++ {
		id = s[id].parent
	}
	return id
}

func msTime(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n)
}

func changedAfter(path string, t time.Time) bool {
	info, err := os.Stat(path)
	return err == nil && info.ModTime().After(t)
}

func (o openCode) Scan(q Query, emit func(Event)) error {
	path := o.db()
	if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
		// a reply finished after Since was written after it, to the
		// database or its write-ahead log
		if !changedAfter(path, q.Since) && !changedAfter(path+"-wal", q.Since) {
			return nil
		}
		return o.scanDB(path, q, emit)
	}
	if o.name == "opencode" {
		o.scanFiles(filepath.Join(openCodeDir(), "storage"), q, emit)
	}
	return nil
}

// openReadOnly opens another program's database without writing to it, or
// making the -wal and -shm files a read of a WAL database needs when there
// are none: then everything is in the file, which is read as it stands.
func openReadOnly(path string) (*sql.DB, error) {
	query := "mode=ro&_pragma=busy_timeout(1000)"
	if _, err := os.Stat(path + "-wal"); err != nil {
		query += "&immutable=1"
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: p, RawQuery: query}).String())
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func hasTable(db *sql.DB, name string) bool {
	var n int
	return db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n) == nil && n > 0
}

// ocMigrated says whether OpenCode 2 has copied every old session over.
func ocMigrated(db *sql.DB) bool {
	var b []byte
	if db.QueryRow(`SELECT value FROM kv WHERE key = 'migration.v1-v2'`).Scan(&b) != nil {
		return false
	}
	var v struct {
		Phase string `json:"phase"`
	}
	return json.Unmarshal(b, &v) == nil && v.Phase == "completed"
}

func loadSessions(db *sql.DB, query string, into ocSessions) {
	rows, err := db.Query(query)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var s ocSession
		if rows.Scan(&id, &s.parent, &s.dir) == nil {
			into[id] = s
		}
	}
}

func (o openCode) scanDB(path string, q Query, emit func(Event)) error {
	db, err := openReadOnly(path)
	if err != nil {
		return err
	}
	defer db.Close()
	v2 := o.name == "opencode" && hasTable(db, "session_v2")
	old, notCopied := hasTable(db, "session") && hasTable(db, "message"), ""
	if v2 && old {
		if ocMigrated(db) {
			old = false
		} else {
			notCopied = ` AND session_id NOT IN (SELECT id FROM session_v2)`
		}
	}
	sessions := ocSessions{}
	if old {
		loadSessions(db, `SELECT id, COALESCE(parent_id, ''), COALESCE(directory, '') FROM session`, sessions)
	}
	if v2 {
		loadSessions(db, `SELECT id, COALESCE(parent_id, ''), COALESCE(directory, '') FROM session_v2`, sessions)
	}
	// a row is written again as its reply goes on, and last when it ends
	since := q.Since.Add(-time.Minute).UnixMilli()
	if v2 {
		o.v2Messages(db, since, sessions, q, emit)
	}
	if old {
		rows, err := db.Query(`SELECT session_id, time_created, data FROM message WHERE time_updated >= ?`+notCopied, since)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sid string
			var created int64
			var data []byte
			if rows.Scan(&sid, &created, &data) != nil {
				continue
			}
			var m ocMessage
			if json.Unmarshal(data, &m) == nil {
				o.message(sid, created, m, sessions, q, emit)
			}
		}
	}
	return nil
}

// v2Messages reads OpenCode 2's messages. A compaction that names no model
// ran on the one the session last replied with.
func (o openCode) v2Messages(db *sql.DB, since int64, sessions ocSessions, q Query, emit func(Event)) {
	rows, err := db.Query(`SELECT session_id, type, seq, time_created, data FROM session_message
		WHERE type IN ('user', 'assistant', 'compaction') AND time_updated >= ? ORDER BY session_id, seq`, since)
	if err != nil {
		return
	}
	type pending struct {
		sid      string
		seq, at  int64
		m        ocMessage
		provider string
	}
	var later []pending
	last := map[string][2]string{} // session: provider, model last replied with
	for rows.Next() {
		var sid, typ string
		var seq, created int64
		var data []byte
		if rows.Scan(&sid, &typ, &seq, &created, &data) != nil {
			continue
		}
		var m ocMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		m.Role = typ
		switch typ {
		case "assistant":
			if prov, mod := m.model(); mod != "" {
				last[sid] = [2]string{prov, mod}
			}
		case "compaction":
			m.Role = "assistant"
			if _, mod := m.model(); mod == "" {
				if l, ok := last[sid]; ok {
					m.ProviderID, m.ModelID = l[0], l[1]
				} else {
					later = append(later, pending{sid: sid, seq: seq, at: created, m: m})
					continue
				}
			}
		}
		o.message(sid, created, m, sessions, q, emit)
	}
	rows.Close()
	for _, p := range later {
		var data []byte
		if db.QueryRow(`SELECT data FROM session_message WHERE session_id = ? AND type = 'assistant' AND seq < ? ORDER BY seq DESC LIMIT 1`,
			p.sid, p.seq).Scan(&data) != nil {
			continue
		}
		var a ocMessage
		if json.Unmarshal(data, &a) == nil {
			p.m.ProviderID, p.m.ModelID = a.model()
			o.message(p.sid, p.at, p.m, sessions, q, emit)
		}
	}
}

// message turns one message into its event: a reply into a request, at the
// time it ended, and a prompt typed in a session of its own (a subagent's
// is its parent's instructions) into a prompt.
func (o openCode) message(sid string, created int64, m ocMessage, sessions ocSessions, q Query, emit func(Event)) {
	s := sessions[sid]
	cwd := s.dir
	if m.Path != nil && m.Path.Cwd != "" {
		cwd = m.Path.Cwd
	}
	ev := Event{Agent: o.name, Session: sessions.root(sid), Thread: sid, Cwd: cwd}
	switch m.Role {
	case "user":
		ev.Time, ev.Kind = msTime(firstPositive(m.Time.Created, created)), Prompt
		typed := m.Semantics == nil || m.Semantics.Origin == "" || m.Semantics.Origin == "real_user"
		if s.parent == "" && typed && q.holds(ev.Time) {
			emit(ev)
		}
	case "assistant":
		t := m.Tokens
		ev.Provider, ev.Model = m.model()
		ev.Time = msTime(firstPositive(m.Time.Completed, m.Time.Created, created))
		if t == nil || ev.Model == "" || !q.holds(ev.Time) {
			return
		}
		ev.Usage = Usage{Input: t.Input, CacheRead: t.Cache.Read, CacheWrite: t.Cache.Write, Output: t.Output + t.Reasoning}
		if o.name == "zcode" {
			ev.Usage.Input, ev.Usage.Output = max(0, t.Input-t.Cache.Read-t.Cache.Write), t.Output
		}
		ev.Reported = m.Cost
		if !ev.Usage.Zero() && ev.Usage.Input >= 0 && ev.Usage.Output >= 0 {
			emit(ev)
		}
	}
}

func firstPositive(v ...int64) int64 {
	for _, n := range v {
		if n > 0 {
			return n
		}
	}
	return 0
}

// ocInfo is a session's file under storage/session/<project>/.
type ocInfo struct {
	ID        string `json:"id"`
	ParentID  string `json:"parentID"`
	Directory string `json:"directory"`
}

// scanFiles reads OpenCode's sessions from before its database: a message's
// file is written again as its reply goes on, so only those changed after
// Since are read.
func (o openCode) scanFiles(root string, q Query, emit func(Event)) {
	files := recentFiles(filepath.Join(root, "message"), q.Since, func(path string) bool {
		return strings.HasSuffix(path, ".json")
	})
	if len(files) == 0 {
		return
	}
	sessions := ocSessions{}
	var info func(sid string)
	info = func(sid string) {
		if _, ok := sessions[sid]; ok || sid == "" {
			return
		}
		sessions[sid] = ocSession{}
		ps, _ := filepath.Glob(filepath.Join(root, "session", "*", sid+".json"))
		for _, p := range ps {
			var i ocInfo
			if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &i) == nil {
				sessions[sid] = ocSession{parent: i.ParentID, dir: i.Directory}
				info(i.ParentID)
				return
			}
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		var m ocMessage
		if err != nil || json.Unmarshal(b, &m) != nil {
			continue
		}
		sid := filepath.Base(filepath.Dir(f))
		info(sid)
		o.message(sid, 0, m, sessions, q, emit)
	}
}
