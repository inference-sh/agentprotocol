package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/inference-sh/agentprotocol/transcript"
)

// store is one SQLite codec with its captured sample, for the checks every
// such codec runs.
type store struct {
	name  string
	codec transcript.Codec
	home  string // testdata home
	db    string // database path, relative to home
	id    string
}

var stores = []store{
	{"goose", Goose, "testdata/goose", goosePath, gooseID},
	{"hermes", Hermes, "testdata/hermes", hermesPath, hermesID},
	{"opencode", Opencode, "testdata/opencode", ".local/share/opencode/opencode.db", opencodeID},
	{"kilo", Kilo, "testdata/kilo", ".local/share/kilo/kilo.db", kiloID},
	{"cursor", Cursor, "testdata/cursor", filepath.Join(cursorChats, cursorDir(cursorCWD), cursorID, "store.db"), cursorID},
}

// copyHome copies a sample home into a temporary one, so a test can write
// to the agent's real database without touching testdata.
func copyHome(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-wal") {
			return nil
		}
		return copyFile(path, target)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// dump reads every row of every table, in rowid order, as comparable text.
func dump(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, done, err := openRO(path)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	defer db.Close()
	names, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND sql NOT LIKE 'CREATE VIRTUAL%' AND name NOT LIKE '%\_data' ESCAPE '\' AND name NOT LIKE '%\_idx' ESCAPE '\' AND name NOT LIKE '%\_content' ESCAPE '\' AND name NOT LIKE '%\_docsize' ESCAPE '\' AND name NOT LIKE '%\_config' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for names.Next() {
		var n string
		if err := names.Scan(&n); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, n)
	}
	names.Close()
	out := map[string][]string{}
	for _, table := range tables {
		rows, err := db.Query(fmt.Sprintf("SELECT * FROM [%s]", table))
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			var b strings.Builder
			for i, v := range vals {
				if bs, ok := v.([]byte); ok {
					v = string(bs)
				}
				fmt.Fprintf(&b, "%s=%v;", cols[i], v)
			}
			out[table] = append(out[table], b.String())
		}
		rows.Close()
		sort.Strings(out[table])
	}
	return out
}

// TestUnchangedRewrite reads each sample session and writes it back with no
// change. Every row of every table must be exactly what it was: a rewrite
// that alters an agent's own session can leave it unloadable, as the harness
// seed probe found for opencode.
func TestUnchangedRewrite(t *testing.T) {
	for _, st := range stores {
		t.Run(st.name, func(t *testing.T) {
			home := copyHome(t, st.home)
			before := dump(t, filepath.Join(home, st.db))
			s, err := mustOpen(t, st.codec, home).Read(t.Context(), st.id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mustOpen(t, st.codec, home).Write(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			after := dump(t, filepath.Join(home, st.db))
			for table, rows := range before {
				if strings.Join(rows, "\n") != strings.Join(after[table], "\n") {
					t.Errorf("table %s changed on an unchanged rewrite\nbefore:\n%s\nafter:\n%s", table, strings.Join(rows, "\n"), strings.Join(after[table], "\n"))
				}
			}
			for table := range after {
				if _, ok := before[table]; !ok {
					t.Errorf("unchanged rewrite added table %s", table)
				}
			}
		})
	}
}

// TestAppendInPlace reads each sample session, adds a user turn and an
// answer, and writes it into the agent's own database. Every row that was
// there must still be there unchanged, apart from the session's own row,
// which may record the update; the new turn must read back last.
func TestAppendInPlace(t *testing.T) {
	for _, st := range stores {
		t.Run(st.name, func(t *testing.T) {
			home := copyHome(t, st.home)
			before := dump(t, filepath.Join(home, st.db))
			s, err := mustOpen(t, st.codec, home).Read(t.Context(), st.id)
			if err != nil {
				t.Fatal(err)
			}
			s.Entries = append(s.Entries,
				transcript.Entry{Role: transcript.RoleUser, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "The codename is HERON. Remember it."}}},
				transcript.Entry{Role: transcript.RoleAssistant, Content: []transcript.Block{{Kind: transcript.BlockText, Text: "Noted: HERON."}}},
			)
			if _, err := mustOpen(t, st.codec, home).Write(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			after := dump(t, filepath.Join(home, st.db))
			for table, rows := range before {
				// The session's own row may record the update, cursor's meta
				// row moves to the new root, and sqlite_sequence is SQLite's
				// autoincrement counter, which inserting must advance.
				if table == "session" || table == "sessions" || table == "meta" || table == "sqlite_sequence" {
					continue
				}
				have := map[string]bool{}
				for _, r := range after[table] {
					have[r] = true
				}
				for _, r := range rows {
					if !have[r] {
						t.Errorf("table %s lost or changed a row on append: %s", table, r)
					}
				}
			}
			back, err := mustOpen(t, st.codec, home).Read(t.Context(), s.ID)
			if err != nil {
				t.Fatal(err)
			}
			msgs := back.Linearize()
			if len(msgs) < 2 || msgs[len(msgs)-2].Text() != "The codename is HERON. Remember it." || msgs[len(msgs)-1].Text() != "Noted: HERON." {
				t.Errorf("appended turn is not last on read back: %+v", msgs[max(0, len(msgs)-2):])
			}
		})
	}
}

func mustOpen(t *testing.T, c transcript.Codec, home string) transcript.Store {
	t.Helper()
	st, err := c.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
