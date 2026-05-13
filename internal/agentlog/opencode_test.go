package agentlog

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestParseOpenCodePart_Text(t *testing.T) {
	row := openCodePartRow{
		ID:          "prt_1",
		TimeCreated: 1700000000000,
		Data:        `{"type":"text","text":"hello","time":{"start":1700000000000,"end":1700000000123}}`,
		MessageData: `{"role":"assistant"}`,
	}

	events := parseOpenCodePart(row, "gt-test", "opencode", "ses_1")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventType != "text" || events[0].Role != "assistant" || events[0].Content != "hello" {
		t.Fatalf("unexpected event: %+v", events[0])
	}
}

func TestParseOpenCodePart_Tool(t *testing.T) {
	row := openCodePartRow{
		ID:          "prt_1",
		TimeCreated: 1700000000000,
		Data:        `{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"pwd"},"output":"/tmp"}}`,
		MessageData: `{"role":"assistant"}`,
	}

	events := parseOpenCodePart(row, "gt-test", "opencode", "ses_1")
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[0].EventType != "tool_use" || events[0].Content != `bash: {"command":"pwd"}` {
		t.Fatalf("unexpected tool_use: %+v", events[0])
	}
	if events[1].EventType != "tool_result" || events[1].Content != "/tmp" {
		t.Fatalf("unexpected tool_result: %+v", events[1])
	}
}

func TestParseOpenCodeUsage(t *testing.T) {
	row := openCodeMessageRow{
		ID:          "msg_1",
		TimeUpdated: 1700000000999,
		Data:        `{"role":"assistant","tokens":{"input":10,"output":20,"cache":{"read":30,"write":40}},"time":{"completed":1700000000123}}`,
	}

	events := parseOpenCodeUsage(row, "gt-test", "opencode", "ses_1")
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "usage" || ev.InputTokens != 10 || ev.OutputTokens != 20 || ev.CacheReadTokens != 30 || ev.CacheCreationTokens != 40 {
		t.Fatalf("unexpected usage event: %+v", ev)
	}
}

func TestOpenCodeWatchEmitsEventsFromDB(t *testing.T) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 not available")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")
	workDir := filepath.Join(dir, "work")
	if err := os.MkdirAll(workDir, 0755); err != nil {
		t.Fatal(err)
	}

	schema := `
create table session (id text primary key, directory text not null, time_created integer not null, time_updated integer not null);
create table message (id text primary key, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
create table part (id text primary key, message_id text not null, session_id text not null, time_created integer not null, time_updated integer not null, data text not null);
insert into session values ('ses_test', '` + sqliteLiteralForTest(workDir) + `', 1700000000000, 1700000000000);
insert into message values ('msg_user', 'ses_test', 1700000000001, 1700000000001, '{"role":"user","time":{"created":1700000000001}}');
insert into part values ('prt_user', 'msg_user', 'ses_test', 1700000000002, 1700000000002, '{"type":"text","text":"start"}');
insert into message values ('msg_assistant', 'ses_test', 1700000000100, 1700000000200, '{"role":"assistant","tokens":{"input":1,"output":2,"cache":{"read":3,"write":4}},"time":{"created":1700000000100,"completed":1700000000200}}');
insert into part values ('prt_assistant', 'msg_assistant', 'ses_test', 1700000000101, 1700000000201, '{"type":"text","text":"done","time":{"start":1700000000101,"end":1700000000201}}');
`
	if out, err := exec.Command("sqlite3", dbPath, schema).CombinedOutput(); err != nil {
		t.Fatalf("creating sqlite db: %v: %s", err, out)
	}

	t.Setenv("OPENCODE_DB_PATH", dbPath)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	adapter := &OpenCodeAdapter{}
	ch, err := adapter.Watch(ctx, "gt-test", workDir, time.UnixMilli(1699999999000))
	if err != nil {
		t.Fatalf("Watch returned error: %v", err)
	}

	var events []AgentEvent
	deadline := time.After(3 * time.Second)
	for len(events) < 3 {
		select {
		case ev := <-ch:
			events = append(events, ev)
		case <-deadline:
			t.Fatalf("timed out waiting for events, got %+v", events)
		}
	}
	cancel()

	if events[0].NativeSessionID != "ses_test" || events[0].Content != "start" {
		t.Fatalf("unexpected first event: %+v", events[0])
	}
	if events[1].Content != "done" {
		t.Fatalf("unexpected assistant text event: %+v", events[1])
	}
	if events[2].EventType != "usage" || events[2].InputTokens != 1 || events[2].OutputTokens != 2 {
		t.Fatalf("unexpected usage event: %+v", events[2])
	}
}

func sqliteLiteralForTest(s string) string {
	return sqliteString(s)[1 : len(sqliteString(s))-1]
}
