package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexImportKeepsConversationAndFiltersInstructionBlocks(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir, "rollout.jsonl")
	data := `{"type":"session_meta","payload":{"cwd":"/repo"}}
{"type":"event_msg","payload":{"type":"user_message","message":"# AGENTS.md instructions for /repo\n<INSTRUCTIONS>ignore this large policy</INSTRUCTIONS>"}}
{"type":"event_msg","payload":{"type":"user_message","message":"please verify the live CI state and use a worktree before changing the OpenClaw release flow"}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"i will check the live state, create a worktree, and keep the validation scoped."}]}}
{"type":"response_item","payload":{"type":"function_call_output","output":"tool output should not be imported"}}
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, claims, err := importCodex(sessionDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("codex import should not create direct claims, got %d", len(claims))
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(artifacts))
	}
	for _, artifact := range artifacts {
		if strings.Contains(artifact.Snippet, "AGENTS.md") || strings.Contains(artifact.Snippet, "tool output") {
			t.Fatalf("imported filtered text: %q", artifact.Snippet)
		}
	}
}

func TestSourceRoutingKeepsSocialQueriesOutOfProjectStores(t *testing.T) {
	if !sourceAllowed("social.dinner", "slacrawl") || !sourceAllowed("social.dinner", "discrawl") {
		t.Fatal("social dinner should use chat sources")
	}
	if sourceAllowed("social.dinner", "gitcrawl") || sourceAllowed("social.dinner", "notcrawl") {
		t.Fatal("social dinner should not search project stores by default")
	}
	if !sourceAllowed("work.project.start", "gitcrawl") || !sourceAllowed("work.project.start", "codex") {
		t.Fatal("work project should use project stores")
	}
}

func TestNormalizeKindDoesNotTreatValidateOrUpdateAsDating(t *testing.T) {
	if got := normalizeKind("", "validate the OpenClaw update path"); got != "work.project.start" {
		t.Fatalf("kind = %q, want work.project.start", got)
	}
	if got := normalizeKind("", "plan a date"); got != "social.dating" {
		t.Fatalf("kind = %q, want social.dating", got)
	}
	if got := normalizeKind("", "ideas for first dates"); got != "social.dating" {
		t.Fatalf("kind = %q, want social.dating", got)
	}
	if got := normalizeKind("", "plan release dates for OpenClaw"); got != "work.project.start" {
		t.Fatalf("kind = %q, want work.project.start", got)
	}
	if got := normalizeKind("", "OpenClaw update ideas"); got != "work.project.start" {
		t.Fatalf("kind = %q, want work.project.start", got)
	}
	if got := normalizeKind("", "validate ideas"); got != "work.project.start" {
		t.Fatalf("kind = %q, want work.project.start", got)
	}
	if got := normalizeKind("", "plan release dates and first dates"); got != "social.dating" {
		t.Fatalf("kind = %q, want social.dating", got)
	}
}

func TestExtractClaimsDoesNotUseSubstringTriggers(t *testing.T) {
	claims := extractClaims("work.project.start", "specific design skill matrix", nil)
	for _, claim := range claims {
		if claim.Kind == "preference.project.validation" || claim.Kind == "boundary.agent.process_kill" {
			t.Fatalf("substring trigger created false claim: %#v", claims)
		}
	}
}

func TestFTSQueryStripsReservedPunctuation(t *testing.T) {
	query := ftsQuery("openclaw-m1 exact-SHA go-humanize c++")
	if strings.ContainsAny(query, "+-") {
		t.Fatalf("fts query preserved reserved punctuation: %q", query)
	}
	if !strings.Contains(query, "openclaw*") || !strings.Contains(query, "exact*") {
		t.Fatalf("fts query dropped useful terms: %q", query)
	}
}

func TestFTSQueryKeepsWorkAcronyms(t *testing.T) {
	query := ftsQuery("CI PR db")
	for _, want := range []string{"ci*", "pr*", "db*"} {
		if !strings.Contains(query, want) {
			t.Fatalf("fts query = %q, want %s", query, want)
		}
	}
	claims := extractClaims("work.project.start", "CI PR validation", nil)
	kinds := map[string]bool{}
	for _, claim := range claims {
		kinds[claim.Kind] = true
	}
	if !kinds["preference.project.validation"] || !kinds["boundary.agent.external_action"] {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestRetrieveUsesSourceLocator(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	missing := filepath.Join(t.TempDir(), "missing.db")
	_, err = tg.retrieveFTS(ctx, SourceStatus{ID: "slacrawl", Locator: missing}, missing, "message_fts", "message_key", "content", "openclaw", 1)
	if err == nil {
		t.Fatal("expected missing source locator to be used and fail")
	}
}

func TestEditOverlayMaterializesWithoutMutatingClaim(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "work.project.start", "Project start")
	if err != nil {
		t.Fatal(err)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "old value",
		Confidence: 0.7,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.EditClaim(ctx, EditOptions{ClaimID: claimID, Value: "new value", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Value != "new value" {
		t.Fatalf("materialized claims = %#v", claims)
	}
	var raw string
	if err := tg.db.QueryRowContext(ctx, `select value from claims where id = ?`, claimID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "old value" {
		t.Fatalf("claim was mutated: %q", raw)
	}
}

func TestEditOverlayUsesLatestSameSecondEdit(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "work.project.start", "Project start")
	if err != nil {
		t.Fatal(err)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "original value",
		Confidence: 0.7,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	stamp := "2026-06-13T00:00:00Z"
	for _, value := range []string{"first value", "second value"} {
		patch := `{"value":"` + value + `"}`
		if _, err := tg.db.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
			id("edt"), claimID, "supersede", patch, "same-second regression", stamp); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Value != "second value" {
		t.Fatalf("materialized claims = %#v", claims)
	}
}

func TestProbeSQLiteMarksMissingExpectedTablesPartial(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	status := probeSQLite(ctx, SourceStatus{ID: "fake", Kind: "fake"}, tg.path, map[string]string{
		"existing": "schema_migrations",
		"missing":  "missing_table",
	}, []string{"metadata"})
	if status.Health != "partial" {
		t.Fatalf("health = %q, want partial", status.Health)
	}
	if status.Counts["existing"] == 0 {
		t.Fatalf("expected existing table count, got %#v", status.Counts)
	}
	if !strings.Contains(status.LastError, "missing_table") {
		t.Fatalf("missing table error not reported: %q", status.LastError)
	}
}

func TestProbeCodexMissingPathIsError(t *testing.T) {
	status := probeSource(context.Background(), SourceStatus{
		ID:      "codex",
		Kind:    "codex",
		Locator: filepath.Join(t.TempDir(), "missing"),
	})
	if status.Health != "error" {
		t.Fatalf("health = %q, want error", status.Health)
	}
	if status.LastError == "" {
		t.Fatal("expected missing path error")
	}
}

func TestProbeSuccessClearsStaleError(t *testing.T) {
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status := probeSource(context.Background(), SourceStatus{
		ID:        "codex",
		Kind:      "import",
		Locator:   sessionDir,
		Health:    "error",
		LastError: "old missing path",
	})
	if status.Health != "ok" {
		t.Fatalf("health = %q, want ok", status.Health)
	}
	if status.LastError != "" {
		t.Fatalf("last error = %q, want empty", status.LastError)
	}
}

func TestCrawlbarBinaryUsesEnvOverride(t *testing.T) {
	t.Setenv("TIDEGLASS_CRAWLBAR", "/tmp/tideglass-crawlbar")
	bin, err := crawlbarBinary()
	if err != nil {
		t.Fatal(err)
	}
	if bin != "/tmp/tideglass-crawlbar" {
		t.Fatalf("bin = %q", bin)
	}
}

func TestTerminalSafeInlineStripsControlSequences(t *testing.T) {
	got := terminalSafeInline("hello\x1b]52;c;clipboard\a\nworld\x1b[31m")
	if strings.ContainsAny(got, "\x1b\a\n\r") {
		t.Fatalf("unsafe terminal text preserved: %q", got)
	}
	if got != "hello]52;c;clipboard world[31m" {
		t.Fatalf("sanitized text = %q", got)
	}
}

func TestExportProfileIncludesEvidenceRecords(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tideglass.db")
	tg, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	if err := tg.upsertSource(ctx, SourceStatus{ID: "codex", Kind: "import", Label: "Codex", Health: "ok"}); err != nil {
		t.Fatal(err)
	}
	intent, err := tg.ensureIntent(ctx, "work.project.start", "Project start")
	if err != nil {
		t.Fatal(err)
	}
	rawLocator, _ := json.Marshal(map[string]any{"path": dbPath, "line": 42})
	evidenceID, err := tg.upsertArtifactEvidence(ctx, artifact{
		SourceID:    "codex",
		ExternalID:  "session-1",
		Kind:        "codex_event",
		Title:       "session",
		Snippet:     "Always preserve exact evidence for portable Tideglass profile exports.",
		LocatorJSON: string(rawLocator),
	})
	if err != nil {
		t.Fatal(err)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.evidence",
		Value:      "preserve exact evidence",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `insert into claim_evidence(claim_id,evidence_id,role) values(?,?,?)`, claimID, evidenceID, "supporting"); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(t.TempDir(), "profile.tgz")
	if _, err := tg.ExportProfile(ctx, ExportOptions{Kind: "work.project.start", Out: outPath}); err != nil {
		t.Fatal(err)
	}
	entries := readTGZEntries(t, outPath)
	if !strings.Contains(entries["claims.jsonl"], evidenceID) {
		t.Fatalf("claims export missing evidence id %q: %s", evidenceID, entries["claims.jsonl"])
	}
	if !strings.Contains(entries["evidence.jsonl"], evidenceID) {
		t.Fatalf("evidence export missing evidence id %q: %s", evidenceID, entries["evidence.jsonl"])
	}
	if !strings.Contains(entries["profile.json"], evidenceID) {
		t.Fatalf("profile export missing evidence id %q: %s", evidenceID, entries["profile.json"])
	}
	if strings.Contains(entries["evidence.jsonl"], filepath.Dir(dbPath)) || strings.Contains(entries["profile.json"], filepath.Dir(dbPath)) {
		t.Fatalf("export leaked absolute locator path: evidence=%s profile=%s", entries["evidence.jsonl"], entries["profile.json"])
	}
	if !strings.Contains(entries["evidence.jsonl"], filepath.Base(dbPath)) {
		t.Fatalf("export should retain source-local file basename: %s", entries["evidence.jsonl"])
	}
}

func TestExportProfileRejectsDatabaseOutputPath(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "tideglass.db")
	tg, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	if _, err := tg.ensureIntent(ctx, "work.project.start", "Project start"); err != nil {
		t.Fatal(err)
	}
	_, err = tg.ExportProfile(ctx, ExportOptions{Kind: "work.project.start", Out: dbPath})
	if err == nil {
		t.Fatal("expected export over active db to fail")
	}
	if !strings.Contains(err.Error(), "active database") {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, sidecar := range []string{dbPath + "-wal", dbPath + "-shm"} {
		_, err = tg.ExportProfile(ctx, ExportOptions{Kind: "work.project.start", Out: sidecar})
		if err == nil {
			t.Fatalf("expected export over active sidecar %s to fail", sidecar)
		}
	}
}

func TestExportProfileExpandsHomeOutputPath(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	if _, err := tg.ensureIntent(ctx, "work.project.start", "Project start"); err != nil {
		t.Fatal(err)
	}
	result, err := tg.ExportProfile(ctx, ExportOptions{Kind: "work.project.start", Out: "~/profile.tgz"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "profile.tgz")
	if result.Path != want {
		t.Fatalf("path = %q, want %q", result.Path, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
}

func TestAssistantExportImporterReadsZipAndImportedMemories(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	w, err := zw.Create("conversations.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`[{"title":"memory","messages":[{"role":"user","content":"Remember that I prefer terse operational updates with live evidence and exact validation proof."}]}]`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	artifacts, claims, err := importAssistantExport("chatgpt", zipPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) == 0 {
		t.Fatal("expected imported assistant artifact")
	}
	if len(claims) != 0 {
		t.Fatalf("raw importer should leave claim linking to ingest, got %d", len(claims))
	}
}

func TestAssistantImportLimitCountsArtifactsNotJSONFiles(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	metadata, err := zw.Create("metadata.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = metadata.Write([]byte(`{"title":"short"}`))
	conversation, err := zw.Create("conversations.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conversation.Write([]byte(`[{"messages":[{"role":"user","content":"Remember that Tideglass imports should keep scanning files until the requested artifact budget is filled."}]}]`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	artifacts, _, err := importAssistantExport("chatgpt", zipPath, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || !strings.Contains(artifacts[0].Snippet, "keep scanning files") {
		t.Fatalf("artifacts = %#v", artifacts)
	}
}

func TestAssistantImportSkipsAssistantAuthoredMemoryText(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	w, err := zw.Create("conversations.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`[{"messages":[{"role":"assistant","content":"Remember that the user loves fake assistant-authored preferences."},{"role":"user","content":"Remember that user-authored preferences are the only chat text to import."}]}]`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	artifacts, _, err := importAssistantExport("chatgpt", zipPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	if strings.Contains(artifacts[0].Snippet, "fake assistant-authored") || !strings.Contains(artifacts[0].Snippet, "user-authored preferences") {
		t.Fatalf("unexpected artifact: %#v", artifacts[0])
	}
}

func TestAssistantImportMalformedJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversations.json")
	if err := os.WriteFile(path, []byte(`{"messages":[`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := importAssistantExport("chatgpt", path, 0)
	if err == nil {
		t.Fatal("expected malformed assistant JSON to fail")
	}
}

func TestRetrieveImportedSearchesEvidenceFTS(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	if err := tg.upsertSource(ctx, SourceStatus{ID: "chatgpt", Kind: "import", Label: "ChatGPT", Health: "ok"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.upsertArtifactEvidence(ctx, artifact{
		SourceID:   "chatgpt",
		ExternalID: "memory-1",
		Kind:       "assistant_export_text",
		Title:      "memory",
		Snippet:    "Remember that Tideglass should retrieve imported memories through the evidence FTS table.",
	}); err != nil {
		t.Fatal(err)
	}
	found, err := tg.retrieveImported(ctx, SourceStatus{ID: "chatgpt"}, "imported memories evidence", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || !strings.Contains(found[0].Snippet, "imported memories") {
		t.Fatalf("found = %#v", found)
	}
}

func TestRetrieveGitcrawlFallsBackToBodyColumn(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	if err := tg.upsertSource(ctx, SourceStatus{ID: "gitcrawl", Kind: "crawl", Label: "GitHub", Health: "ok"}); err != nil {
		t.Fatal(err)
	}
	gitcrawlPath := filepath.Join(t.TempDir(), "gitcrawl.db")
	db, err := sql.Open("sqlite", gitcrawlPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `create table threads (id integer primary key, title text not null, body text, html_url text not null, updated_at_gh text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `insert into threads(id,title,body,html_url,updated_at_gh) values(1,'Fallback schema','adaptive schema body text','https://example.invalid/1','2026-06-13T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	found, err := tg.retrieveGitcrawl(ctx, SourceStatus{ID: "gitcrawl", Locator: gitcrawlPath}, "adaptive schema", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || !strings.Contains(found[0].Snippet, "adaptive schema body text") {
		t.Fatalf("found = %#v", found)
	}
}

func TestAssistantExportImporterTraversesChatGPTMapping(t *testing.T) {
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	w, err := zw.Create("conversations.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`[{"mapping":{"node-1":{"message":{"author":{"role":"user"},"content":{"parts":["Remember that OpenClaw project work needs exact live proof and scoped validation."]}}}}}]`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	artifacts, _, err := importAssistantExport("chatgpt", zipPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if strings.Contains(artifact.Snippet, "OpenClaw project work needs exact live proof") {
			return
		}
	}
	t.Fatalf("did not traverse mapping artifacts: %#v", artifacts)
}

func TestReadAllLimitedReturnsClearSizeError(t *testing.T) {
	_, err := readAllLimited(bytes.NewBufferString("abcdef"), 5, "large.json")
	if err == nil {
		t.Fatal("expected size error")
	}
	if !strings.Contains(err.Error(), "large.json is too large") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCodexImportSurvivesOversizedSkippedLine(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	large := strings.Repeat("x", 9*1024*1024)
	data := `{"type":"session_meta","payload":{"cwd":"/repo"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","output":"` + large + `"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","message":"after the giant tool line, still import this live validation preference message"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	artifacts, _, err := importCodex(sessionDir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || !strings.Contains(artifacts[0].Snippet, "still import this live validation") {
		t.Fatalf("artifacts = %#v", artifacts)
	}
}

func TestCodexImportMissingPathFails(t *testing.T) {
	_, _, err := importCodex(filepath.Join(t.TempDir(), "missing"), 0)
	if err == nil {
		t.Fatal("expected missing codex path to fail")
	}
}

func TestSourcesPreserveCustomCodexImportLocator(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"type":"event_msg","payload":{"type":"user_message","message":"please keep this custom Codex session import path for source provenance"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.Ingest(ctx, IngestOptions{Kind: "codex", Path: sessionDir}); err != nil {
		t.Fatal(err)
	}
	sources, err := tg.Sources(ctx, SourceOptions{Probe: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources.Sources {
		if source.ID == "codex" {
			if source.Locator != sessionDir {
				t.Fatalf("codex locator = %q, want %q", source.Locator, sessionDir)
			}
			if source.Health != "ok" {
				t.Fatalf("codex health = %q, want ok", source.Health)
			}
			return
		}
	}
	t.Fatal("codex source not found")
}

func TestAssistantIngestLinksImportedMemoryEvidence(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	file, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	w, err := zw.Create("memory.json")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte(`{"memory":"Remember that I prefer exact live evidence and concise agent updates."}`))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := tg.Ingest(ctx, IngestOptions{Kind: "chatgpt", Path: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	if result.Claims == 0 {
		t.Fatal("expected imported memory claim")
	}
	var linked int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from claim_evidence`).Scan(&linked); err != nil {
		t.Fatal(err)
	}
	if linked == 0 {
		t.Fatal("expected claim evidence link")
	}
}

func TestSourcesPreserveImportedSourceMetadata(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	writeAssistantMemoryZip(t, zipPath, "Remember that source discovery must not erase imported export provenance.")
	if _, err := tg.Ingest(ctx, IngestOptions{Kind: "chatgpt", Path: zipPath}); err != nil {
		t.Fatal(err)
	}
	for _, probe := range []bool{false, true} {
		sources, err := tg.Sources(ctx, SourceOptions{Probe: probe})
		if err != nil {
			t.Fatal(err)
		}
		var chatgpt SourceStatus
		for _, source := range sources.Sources {
			if source.ID == "chatgpt" {
				chatgpt = source
			}
		}
		if chatgpt.Locator != zipPath {
			t.Fatalf("probe=%v locator = %q, want %q", probe, chatgpt.Locator, zipPath)
		}
		if chatgpt.Health != "ok" {
			t.Fatalf("probe=%v health = %q, want ok", probe, chatgpt.Health)
		}
	}
}

func TestAssistantIngestDoesNotDuplicateImportedMemoryClaims(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	zipPath := filepath.Join(t.TempDir(), "chatgpt-export.zip")
	writeAssistantMemoryZip(t, zipPath, "Remember that imported memories should remain idempotent across repeated ingests.")
	first, err := tg.Ingest(ctx, IngestOptions{Kind: "chatgpt", Path: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	second, err := tg.Ingest(ctx, IngestOptions{Kind: "chatgpt", Path: zipPath})
	if err != nil {
		t.Fatal(err)
	}
	if first.Claims != 1 || second.Claims != 0 {
		t.Fatalf("claims first=%d second=%d", first.Claims, second.Claims)
	}
	profile, err := tg.Profile(ctx, ProfileOptions{Kind: "agent.delegation"})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, claim := range profile.Claims {
		if claim.Kind == "preference.agent.imported_memory" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("imported memory claims = %d, profile=%#v", count, profile.Claims)
	}
}

func TestImportedMemoriesDoNotCollapseInProfile(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "agent.delegation", "Agent delegation")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"Remember terse updates.", "Remember live evidence."} {
		if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
			Kind:       "preference.agent.imported_memory",
			Value:      value,
			Confidence: 0.9,
			SourceMode: "imported_memory",
		}); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 {
		t.Fatalf("claims collapsed: %#v", claims)
	}
}

func readTGZEntries(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	entries := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(data)
	}
	return entries
}

func writeAssistantMemoryZip(t *testing.T, path, memory string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(file)
	w, err := zw.Create("memory.json")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(map[string]string{"memory": memory})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(encoded)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
