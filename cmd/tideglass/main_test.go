package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TIDEGLASS_CRAWLBAR", filepath.Join(home, "no-crawlbar"))
	t.Setenv("CRAWLBAR_BIN", "")
	return home
}

func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	original := *target
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*target = writer
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(reader)
		done <- data
	}()
	defer func() { *target = original }()
	fn()
	_ = writer.Close()
	return string(<-done)
}

func runOK(t *testing.T, args ...string) string {
	t.Helper()
	var runErr error
	out := capture(t, &os.Stdout, func() { runErr = run(context.Background(), args) })
	if runErr != nil {
		t.Fatalf("run %v: %v", args, runErr)
	}
	return out
}

func TestVersionCommands(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if got := strings.TrimSpace(runOK(t, args...)); got != version {
			t.Fatalf("%v printed %q, want %q", args, got, version)
		}
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		var runErr error
		out := capture(t, &os.Stderr, func() { runErr = run(context.Background(), args) })
		if runErr != nil {
			t.Fatalf("%v: %v", args, runErr)
		}
		for _, command := range []string{"init", "sources", "ingest", "ask", "review", "profile show", "profile edit", "profile export", "evidence show", "context", "resolve", "serve", "mcp --once", "doctor", "version"} {
			if !strings.Contains(out, "\n  "+command) {
				t.Fatalf("usage missing %q (or misindented):\n%s", command, out)
			}
		}
	}
}

func TestCommandValidationErrors(t *testing.T) {
	isolate(t)
	db := filepath.Join(t.TempDir(), "tg.db")
	cases := map[string][]string{
		`unknown command "nope"`:              {"nope"},
		"ingest requires a kind":              {"ingest"},
		"ingest --path is required":           {"ingest", "codex", "--db", db},
		"ask requires a query":                {"ask", "--db", db},
		"mcp currently requires --once":       {"mcp", "--db", db},
		"serve --addr must bind to localhost": {"serve", "--db", db, "--addr", "0.0.0.0:8765"},
	}
	for want, args := range cases {
		err := run(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%v err = %v, want %q", args, err, want)
		}
	}
}

func TestInitDoctorAndSourcesAgainstTempDB(t *testing.T) {
	isolate(t)
	db := filepath.Join(t.TempDir(), "tg.db")
	if out := runOK(t, "init", "--db", db); !strings.Contains(out, "initialized "+db) {
		t.Fatalf("init output = %q", out)
	}
	var doctor map[string]any
	if err := json.Unmarshal([]byte(runOK(t, "doctor", "--db", db, "--json")), &doctor); err != nil {
		t.Fatal(err)
	}
	if doctor["db_path"] != db || doctor["overall_state"] == nil {
		t.Fatalf("doctor = %#v", doctor)
	}
	var sources map[string]any
	if err := json.Unmarshal([]byte(runOK(t, "sources", "--json", "--db", db)), &sources); err != nil {
		t.Fatal(err)
	}
	if _, ok := sources["sources"]; !ok {
		t.Fatalf("sources = %#v", sources)
	}
}

func TestIngestThenAskFromTheCLI(t *testing.T) {
	isolate(t)
	db := filepath.Join(t.TempDir(), "tg.db")
	sessions := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"type":"event_msg","payload":{"type":"user_message","message":"please verify the live CI state and use a worktree before changing the OpenClaw release flow"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessions, "rollout.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var ingest map[string]any
	if err := json.Unmarshal([]byte(runOK(t, "ingest", "codex", "--path", sessions, "--db", db, "--json")), &ingest); err != nil {
		t.Fatal(err)
	}
	if ingest["artifacts"].(float64) < 1 {
		t.Fatalf("ingest = %#v", ingest)
	}
	var ask map[string]any
	// flags after the query are accepted thanks to normalizeFlagArgs
	if err := json.Unmarshal([]byte(runOK(t, "ask", "verify", "live", "CI", "--kind", "work.project.start", "--db", db, "--json")), &ask); err != nil {
		t.Fatal(err)
	}
	if ask["query"] != "verify live CI" {
		t.Fatalf("ask = %#v", ask)
	}
}

func TestNormalizeFlagArgsMovesFlagsBeforePositionals(t *testing.T) {
	got := normalizeFlagArgs([]string{"show", "clm_1", "--kind", "social.dinner", "--json", "--db=x.db", "-", "--set", "v"})
	want := []string{"--kind", "social.dinner", "--json", "--db=x.db", "--set", "v", "show", "clm_1", "-"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize = %#v, want %#v", got, want)
	}
	if got := normalizeFlagArgs([]string{"q", "--kind"}); !reflect.DeepEqual(got, []string{"--kind", "q"}) {
		t.Fatalf("trailing value flag = %#v", got)
	}
}

func TestValidateServeAddr(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8765", "localhost:1", "[::1]:9", "LOCALHOST:2"} {
		if err := validateServeAddr(ok); err != nil {
			t.Fatalf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8765", "192.168.1.5:80", "example.com:80", "no-port"} {
		if err := validateServeAddr(bad); err == nil {
			t.Fatalf("%s should be rejected", bad)
		}
	}
}

func TestExpandPathAndSafeLine(t *testing.T) {
	home := isolate(t)
	if got := expandPath("~"); got != home {
		t.Fatalf("~ = %q", got)
	}
	if got := expandPath("~/x/y"); got != filepath.Join(home, "x", "y") {
		t.Fatalf("~/x/y = %q", got)
	}
	if got := expandPath("/abs/~/p"); got != "/abs/~/p" {
		t.Fatalf("abs = %q", got)
	}
	if got := safeLine("a\x1b[31m\tb\n\nc\x7f\u0085 d"); got != "a[31m b c d" {
		t.Fatalf("safeLine = %q", got)
	}
}
