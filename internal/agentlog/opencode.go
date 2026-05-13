package agentlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OpenCodeAdapter watches OpenCode's SQLite conversation store.
type OpenCodeAdapter struct{}

func (a *OpenCodeAdapter) AgentType() string { return "opencode" }

// Watch starts polling OpenCode's SQLite database for the active session in workDir.
// OpenCode records sessions globally, so workDir plus since is used to distinguish
// the Gas Town-launched runtime from older user sessions in the same project.
func (a *OpenCodeAdapter) Watch(ctx context.Context, sessionID, workDir string, since time.Time) (<-chan AgentEvent, error) {
	dbPath, err := openCodeDBPath()
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		return nil, fmt.Errorf("sqlite3 is required to watch OpenCode logs: %w", err)
	}

	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolving absolute work dir: %w", err)
	}

	ch := make(chan AgentEvent, 64)
	go func() {
		defer close(ch)

		for ctx.Err() == nil {
			ocSession, err := waitForOpenCodeSession(ctx, dbPath, absWorkDir, since)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				since = time.Now().Add(-watchPollInterval)
				continue
			}

			tailOpenCodeSession(ctx, dbPath, *ocSession, sessionID, a.AgentType(), ch)
		}
	}()

	return ch, nil
}

func openCodeDBPath() (string, error) {
	if path := os.Getenv("OPENCODE_DB_PATH"); path != "" {
		return path, nil
	}

	if runtime.GOOS == "windows" {
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", fmt.Errorf("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, "opencode", "opencode.db"), nil
	}

	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home dir: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "opencode", "opencode.db"), nil
	}

	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("getting home dir: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "opencode", "opencode.db"), nil
}

type openCodeSessionRow struct {
	ID          string `json:"id"`
	Directory   string `json:"directory"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
}

type openCodePartRow struct {
	ID          string `json:"id"`
	MessageID   string `json:"message_id"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
	Data        string `json:"data"`
	MessageData string `json:"message_data"`
}

type openCodeMessageRow struct {
	ID          string `json:"id"`
	TimeCreated int64  `json:"time_created"`
	TimeUpdated int64  `json:"time_updated"`
	Data        string `json:"data"`
}

func waitForOpenCodeSession(ctx context.Context, dbPath, workDir string, since time.Time) (*openCodeSessionRow, error) {
	deadline := time.Now().Add(watchFileTimeout)
	for {
		row, err := newestOpenCodeSession(ctx, dbPath, workDir, since)
		if err == nil && row != nil {
			return row, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("timeout: no OpenCode session appeared for %s", workDir)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(watchPollInterval):
		}
	}
}

func newestOpenCodeSession(ctx context.Context, dbPath, workDir string, since time.Time) (*openCodeSessionRow, error) {
	minCreated := since.UnixMilli()
	if since.IsZero() {
		minCreated = 0
	}
	query := fmt.Sprintf(
		"select id, directory, time_created, time_updated from session where directory = %s and time_created >= %d order by time_created desc limit 1",
		sqliteString(workDir), minCreated)
	var rows []openCodeSessionRow
	if err := sqliteJSON(ctx, dbPath, query, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func tailOpenCodeSession(ctx context.Context, dbPath string, ocSession openCodeSessionRow, sessionID, agentType string, ch chan<- AgentEvent) {
	seen := make(map[string]bool)
	lastPartUpdate := ocSession.TimeCreated
	lastMessageUpdate := ocSession.TimeCreated

	for {
		if ctx.Err() != nil {
			return
		}

		if parts, err := openCodePartRows(ctx, dbPath, ocSession.ID, lastPartUpdate); err == nil {
			for _, row := range parts {
				if row.TimeUpdated > lastPartUpdate {
					lastPartUpdate = row.TimeUpdated
				}
				for _, ev := range parseOpenCodePart(row, sessionID, agentType, ocSession.ID) {
					key := row.ID + ":" + ev.EventType
					if seen[key] {
						continue
					}
					seen[key] = true
					select {
					case ch <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}

		if messages, err := openCodeMessageRows(ctx, dbPath, ocSession.ID, lastMessageUpdate); err == nil {
			for _, row := range messages {
				if row.TimeUpdated > lastMessageUpdate {
					lastMessageUpdate = row.TimeUpdated
				}
				for _, ev := range parseOpenCodeUsage(row, sessionID, agentType, ocSession.ID) {
					key := row.ID + ":usage"
					if seen[key] {
						continue
					}
					seen[key] = true
					select {
					case ch <- ev:
					case <-ctx.Done():
						return
					}
				}
			}
		}

		newer, err := newestOpenCodeSession(ctx, dbPath, ocSession.Directory, time.UnixMilli(ocSession.TimeCreated+1))
		if err == nil && newer != nil && newer.ID != ocSession.ID {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(watchPollInterval):
		}
	}
}

func openCodePartRows(ctx context.Context, dbPath, nativeSessionID string, sinceUpdated int64) ([]openCodePartRow, error) {
	query := fmt.Sprintf(`select p.id, p.message_id, p.time_created, p.time_updated, p.data, m.data as message_data
from part p join message m on m.id = p.message_id
where p.session_id = %s and p.time_updated >= %d
order by p.time_updated, p.id limit 500`, sqliteString(nativeSessionID), sinceUpdated)
	var rows []openCodePartRow
	err := sqliteJSON(ctx, dbPath, query, &rows)
	return rows, err
}

func openCodeMessageRows(ctx context.Context, dbPath, nativeSessionID string, sinceUpdated int64) ([]openCodeMessageRow, error) {
	query := fmt.Sprintf(`select id, time_created, time_updated, data
from message
where session_id = %s and time_updated >= %d
order by time_updated, id limit 500`, sqliteString(nativeSessionID), sinceUpdated)
	var rows []openCodeMessageRow
	err := sqliteJSON(ctx, dbPath, query, &rows)
	return rows, err
}

func sqliteJSON(ctx context.Context, dbPath, query string, dest any) error {
	out, err := exec.CommandContext(ctx, "sqlite3", "-readonly", "-json", dbPath, query).Output()
	if err != nil {
		return fmt.Errorf("querying OpenCode database: %w", err)
	}
	if len(out) == 0 {
		out = []byte("[]")
	}
	if err := json.Unmarshal(out, dest); err != nil {
		return fmt.Errorf("parsing OpenCode database output: %w", err)
	}
	return nil
}

func sqliteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

type openCodeMessageData struct {
	Role   string `json:"role"`
	Tokens *struct {
		Input  int `json:"input"`
		Output int `json:"output"`
		Cache  struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type openCodePartData struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Time *struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"time"`
	Tool  string `json:"tool"`
	State *struct {
		Status string          `json:"status"`
		Input  json.RawMessage `json:"input"`
		Output json.RawMessage `json:"output"`
	} `json:"state"`
}

func parseOpenCodePart(row openCodePartRow, sessionID, agentType, nativeSessionID string) []AgentEvent {
	var message openCodeMessageData
	if err := json.Unmarshal([]byte(row.MessageData), &message); err != nil {
		return nil
	}

	var part openCodePartData
	if err := json.Unmarshal([]byte(row.Data), &part); err != nil {
		return nil
	}

	ts := openCodePartTimestamp(row, part)
	base := AgentEvent{
		AgentType:       agentType,
		SessionID:       sessionID,
		NativeSessionID: nativeSessionID,
		Role:            message.Role,
		Timestamp:       ts,
	}

	switch part.Type {
	case "text":
		if part.Text == "" || (message.Role == "assistant" && !openCodePartComplete(part)) {
			return nil
		}
		base.EventType = "text"
		base.Content = part.Text
		return []AgentEvent{base}
	case "reasoning":
		if part.Text == "" || !openCodePartComplete(part) {
			return nil
		}
		base.EventType = "thinking"
		base.Content = part.Text
		return []AgentEvent{base}
	case "tool":
		if part.State == nil || part.State.Status != "completed" {
			return nil
		}
		var events []AgentEvent
		if len(part.State.Input) > 0 && string(part.State.Input) != "null" {
			use := base
			use.EventType = "tool_use"
			use.Content = part.Tool + ": " + string(part.State.Input)
			events = append(events, use)
		}
		if len(part.State.Output) > 0 && string(part.State.Output) != "null" {
			result := base
			result.EventType = "tool_result"
			result.Content = rawJSONText(part.State.Output)
			events = append(events, result)
		}
		return events
	default:
		return nil
	}
}

func parseOpenCodeUsage(row openCodeMessageRow, sessionID, agentType, nativeSessionID string) []AgentEvent {
	var message openCodeMessageData
	if err := json.Unmarshal([]byte(row.Data), &message); err != nil {
		return nil
	}
	if message.Role != "assistant" || message.Tokens == nil || message.Time.Completed == 0 {
		return nil
	}
	if message.Tokens.Input == 0 && message.Tokens.Output == 0 && message.Tokens.Cache.Read == 0 && message.Tokens.Cache.Write == 0 {
		return nil
	}
	return []AgentEvent{{
		AgentType:           agentType,
		SessionID:           sessionID,
		NativeSessionID:     nativeSessionID,
		EventType:           "usage",
		Role:                "assistant",
		Timestamp:           millisTime(message.Time.Completed, row.TimeUpdated),
		InputTokens:         message.Tokens.Input,
		OutputTokens:        message.Tokens.Output,
		CacheReadTokens:     message.Tokens.Cache.Read,
		CacheCreationTokens: message.Tokens.Cache.Write,
	}}
}

func openCodePartComplete(part openCodePartData) bool {
	return part.Time == nil || part.Time.End > 0
}

func openCodePartTimestamp(row openCodePartRow, part openCodePartData) time.Time {
	if part.Time != nil {
		if part.Time.End > 0 {
			return time.UnixMilli(part.Time.End)
		}
		if part.Time.Start > 0 {
			return time.UnixMilli(part.Time.Start)
		}
	}
	return millisTime(row.TimeCreated, row.TimeUpdated)
}

func millisTime(primary, fallback int64) time.Time {
	if primary > 0 {
		return time.UnixMilli(primary)
	}
	if fallback > 0 {
		return time.UnixMilli(fallback)
	}
	return time.Now()
}

func rawJSONText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}
