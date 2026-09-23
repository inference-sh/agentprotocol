package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/inference-sh/agentprotocol/transcript"
	"github.com/inference-sh/agentprotocol/transcript/copilot"
)

func init() { transcript.Register("copilot", Copilot) }

// Copilot is the Copilot CLI session store with its index. Copilot finds a
// session through ~/.copilot/session-store.db, so a written session gets its
// files from copilot.Writer and then a sessions row, one turns row per user
// turn, and the search rows Copilot keeps beside them.
var Copilot transcript.Codec = func() transcript.JSONL {
	c := copilot.Writer
	files := c.After
	c.After = func(ctx context.Context, path string, s *transcript.Session) error {
		if err := files(ctx, path, s); err != nil {
			return err
		}
		// path is <home>/.copilot/session-state/<id>/events.jsonl.
		home := filepath.Dir(filepath.Dir(filepath.Dir(path)))
		return indexCopilot(ctx, filepath.Join(home, "session-store.db"), s)
	}
	return c
}()

// copilotSchemaVersion is the session-store schema the DDL below matches,
// captured from Copilot CLI 1.0.88.
const copilotSchemaVersion = 8

// copilotDDL creates a session store the way Copilot does, for a machine
// where Copilot has not created one yet. It is the schema read from a store
// Copilot wrote, so Copilot's own migrations find it current.
const copilotDDL = `
CREATE TABLE schema_version (version INTEGER NOT NULL);
CREATE TABLE sessions (id TEXT PRIMARY KEY, cwd TEXT, repository TEXT, host_type TEXT, branch TEXT, summary TEXT,
  created_at TEXT DEFAULT (datetime('now')), updated_at TEXT DEFAULT (datetime('now')));
CREATE TABLE turns (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  turn_index INTEGER NOT NULL, user_message TEXT, assistant_response TEXT, timestamp TEXT DEFAULT (datetime('now')),
  UNIQUE(session_id, turn_index));
CREATE TABLE checkpoints (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  checkpoint_number INTEGER NOT NULL, title TEXT, overview TEXT, history TEXT, work_done TEXT, technical_details TEXT,
  important_files TEXT, next_steps TEXT, created_at TEXT DEFAULT (datetime('now')), UNIQUE(session_id, checkpoint_number));
CREATE TABLE session_files (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  file_path TEXT NOT NULL, tool_name TEXT, turn_index INTEGER, first_seen_at TEXT DEFAULT (datetime('now')),
  UNIQUE(session_id, file_path));
CREATE TABLE session_refs (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  ref_type TEXT NOT NULL, ref_value TEXT NOT NULL, turn_index INTEGER, created_at TEXT DEFAULT (datetime('now')),
  UNIQUE(session_id, ref_type, ref_value));
CREATE TABLE forge_trajectory_events (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  tool_call_id TEXT, turn_index INTEGER, event_type TEXT NOT NULL, command TEXT, output TEXT, exit_code INTEGER,
  event_key TEXT, event_value TEXT, created_at TEXT DEFAULT (datetime('now')));
CREATE INDEX idx_forge_trajectory_events_tool_call ON forge_trajectory_events(tool_call_id);
CREATE TABLE assistant_usage_events (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL REFERENCES sessions(id),
  turn_index INTEGER, agent_id TEXT, parent_tool_call_id TEXT, model TEXT NOT NULL, copilot_usage_model TEXT,
  input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER,
  total_nano_aiu INTEGER, request_multiplier REAL, duration_ms INTEGER, time_to_first_token_ms INTEGER, output_ttft_ms REAL,
  inter_token_latency_ms INTEGER, initiator TEXT, api_endpoint TEXT, reasoning_effort TEXT, finish_reason TEXT,
  content_filter_triggered INTEGER, token_details_json TEXT, created_at TEXT DEFAULT (datetime('now')));
CREATE INDEX idx_sessions_repo ON sessions(repository);
CREATE INDEX idx_sessions_cwd ON sessions(cwd);
CREATE INDEX idx_session_files_path ON session_files(file_path);
CREATE INDEX idx_session_refs_type_value ON session_refs(ref_type, ref_value);
CREATE INDEX idx_turns_session ON turns(session_id);
CREATE INDEX idx_checkpoints_session ON checkpoints(session_id);
CREATE INDEX idx_forge_trajectory_events_session ON forge_trajectory_events(session_id, id);
CREATE INDEX idx_assistant_usage_events_session ON assistant_usage_events(session_id, id);
CREATE INDEX idx_assistant_usage_events_session_turn ON assistant_usage_events(session_id, turn_index);
CREATE INDEX idx_assistant_usage_events_model ON assistant_usage_events(model);
CREATE TABLE forge_skill_proposals (id TEXT PRIMARY KEY, repo_owner TEXT, repo_name TEXT, git_root_path TEXT NOT NULL,
  branch_name TEXT NOT NULL, trigger_mode TEXT NOT NULL, status TEXT NOT NULL, fingerprint TEXT, manifest_json TEXT,
  summary_json TEXT, workspace_before_json TEXT, superseded_by TEXT, failure_reason TEXT, created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL);
CREATE INDEX idx_forge_skill_proposals_scope_status ON forge_skill_proposals(git_root_path, branch_name, repo_owner, repo_name, status);
CREATE INDEX idx_forge_skill_proposals_scope_fingerprint ON forge_skill_proposals(git_root_path, branch_name, repo_owner, repo_name, fingerprint);
CREATE VIRTUAL TABLE search_index USING fts5(content, session_id UNINDEXED, source_type UNINDEXED, source_id UNINDEXED);
CREATE TABLE dynamic_context_items (repository TEXT NOT NULL, branch TEXT NOT NULL, src TEXT NOT NULL, name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '', content TEXT NOT NULL DEFAULT '', read_count INTEGER NOT NULL DEFAULT 0,
  count INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (repository, branch, src, name));
CREATE INDEX idx_dynamic_context_repo_branch ON dynamic_context_items(repository, branch);
`

// copilotTurn is one user turn as Copilot's index records it.
type copilotTurn struct {
	user, assistant string
}

// copilotTurns pairs each user message with the assistant text that answers
// it, up to the next user message.
func copilotTurns(s *transcript.Session) []copilotTurn {
	var out []copilotTurn
	for _, e := range s.Linearize() {
		switch e.Role {
		case transcript.RoleUser:
			out = append(out, copilotTurn{user: e.Text()})
		case transcript.RoleAssistant:
			if len(out) > 0 && e.Text() != "" {
				t := &out[len(out)-1]
				if t.assistant != "" {
					t.assistant += "\n"
				}
				t.assistant += e.Text()
			}
		}
	}
	return out
}

func indexCopilot(ctx context.Context, path string, s *transcript.Session) error {
	_, statErr := os.Stat(path)
	fresh := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !fresh {
		return statErr
	}
	db, err := openRW(path)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if fresh {
		if _, err := tx.ExecContext(ctx, copilotDDL); err != nil {
			return fmt.Errorf("copilot: create session store: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, copilotSchemaVersion); err != nil {
			return err
		}
	}
	turns := copilotTurns(s)
	summary := s.Title
	if summary == "" && len(turns) > 0 {
		summary = turns[0].user
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (id, cwd, summary, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET cwd = excluded.cwd, summary = excluded.summary, updated_at = excluded.updated_at`,
		s.ID, s.CWD, summary, copilotStamp(s.Created), copilotStamp(s.Updated)); err != nil {
		return fmt.Errorf("copilot: index session: %w", err)
	}
	for _, q := range []string{`DELETE FROM turns WHERE session_id = ?`, `DELETE FROM search_index WHERE session_id = ?`} {
		if _, err := tx.ExecContext(ctx, q, s.ID); err != nil {
			return fmt.Errorf("copilot: clear index: %w", err)
		}
	}
	for i, t := range turns {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO turns (session_id, turn_index, user_message, assistant_response, timestamp) VALUES (?, ?, ?, ?, ?)`,
			s.ID, i, t.user, nullIfEmpty(t.assistant), copilotStamp(s.Updated)); err != nil {
			return fmt.Errorf("copilot: index turn: %w", err)
		}
		content := strings.TrimSuffix(t.user+"\n"+t.assistant, "\n")
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO search_index (content, session_id, source_type, source_id) VALUES (?, ?, 'turn', ?)`,
			content, s.ID, fmt.Sprintf("%s:turn:%d", s.ID, i)); err != nil {
			return fmt.Errorf("copilot: index search: %w", err)
		}
	}
	return tx.Commit()
}

func copilotStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func nullIfEmpty(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
