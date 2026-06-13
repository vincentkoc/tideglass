package app

import (
	"archive/zip"
	"bytes"
	"context"
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
	_, _ = w.Write([]byte(`[{"title":"memory","messages":[{"content":"Remember that I prefer terse operational updates with live evidence and exact validation proof."}]}]`))
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
	_, _ = w.Write([]byte(`[{"mapping":{"node-1":{"message":{"content":{"parts":["Remember that OpenClaw project work needs exact live proof and scoped validation."]}}}}}]`))
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
