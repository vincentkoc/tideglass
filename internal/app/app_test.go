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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if got := normalizeKind("", "new job start date"); got != "work.new_job" {
		t.Fatalf("kind = %q, want work.new_job", got)
	}
	if got := normalizeKind("", "OpenClaw due date"); got != "work.project.start" {
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

func TestExtractClaimsRequiresKillSignalForProcessBoundary(t *testing.T) {
	claims := extractClaims("work.project.start", "improve the release process", nil)
	for _, claim := range claims {
		if claim.Kind == "boundary.agent.process_kill" {
			t.Fatalf("process-only trigger created false claim: %#v", claims)
		}
	}
	claims = extractClaims("work.project.start", "do not kill broad tmux processes", nil)
	for _, claim := range claims {
		if claim.Kind == "boundary.agent.process_kill" {
			return
		}
	}
	t.Fatalf("kill/process trigger missing: %#v", claims)
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

func TestReviewClaimAcceptsAndRejects(t *testing.T) {
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
	acceptedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "run tests",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejectedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "use too much fluff",
		Confidence: 0.6,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: rejectedID, Action: "reject", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].ID != acceptedID || claims[0].Status != "accepted" {
		t.Fatalf("claims = %#v", claims)
	}
	var editCount int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from edits where claim_id in (?,?)`, acceptedID, rejectedID).Scan(&editCount); err != nil {
		t.Fatal(err)
	}
	if editCount != 2 {
		t.Fatalf("review edits = %d, want 2", editCount)
	}
}

func TestReviewClaimChecksExpectedRevisionZero(t *testing.T) {
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
		Kind:       "preference.project.validation",
		Value:      "run tests",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0 where id = ?`, claimID); err != nil {
		t.Fatal(err)
	}
	expectedRevision := int64(0)
	if _, err := tg.EditClaim(ctx, EditOptions{ClaimID: claimID, Value: "run focused tests", Reason: "concurrent edit"}); err != nil {
		t.Fatal(err)
	}
	result, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "stale review", ExpectedRevision: &expectedRevision})
	if err == nil {
		t.Fatalf("stale revision-zero review succeeded: %#v", result)
	}
	if !strings.Contains(err.Error(), "claim revision changed") {
		t.Fatalf("review error = %v", err)
	}
	var status string
	if err := tg.db.QueryRowContext(ctx, `select status from claims where id = ?`, claimID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("stale review changed status to %s", status)
	}
}

func TestEditClaimChecksExpectedRevision(t *testing.T) {
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
		Kind:       "preference.project.validation",
		Value:      "run tests",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	var expectedRevision int64
	if err := tg.db.QueryRowContext(ctx, `select revision from claims where id = ?`, claimID).Scan(&expectedRevision); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.EditClaim(ctx, EditOptions{ClaimID: claimID, Value: "run newer tests", Reason: "concurrent edit"}); err != nil {
		t.Fatal(err)
	}
	result, err := tg.EditClaim(ctx, EditOptions{ClaimID: claimID, Value: "stale overwrite", Reason: "stale edit", ExpectedRevision: &expectedRevision})
	if err == nil {
		t.Fatalf("stale edit succeeded: %#v", result)
	}
	if !strings.Contains(err.Error(), "claim revision changed") {
		t.Fatalf("edit error = %v", err)
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Value != "run newer tests" {
		t.Fatalf("stale edit overwrote claims: %#v", claims)
	}
}

func TestReviewAcceptKeepsEditedOverlay(t *testing.T) {
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
		Kind:       "preference.project.validation",
		Value:      "old value",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.EditClaim(ctx, EditOptions{ClaimID: claimID, Value: "reviewed value", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Value != "reviewed value" || claims[0].Status != "accepted" {
		t.Fatalf("claims = %#v", claims)
	}
}

func TestRejectedDuplicateSingletonDoesNotRevealOlderActive(t *testing.T) {
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
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.7,
		SourceMode: "inferred",
	}); err != nil {
		t.Fatal(err)
	}
	duplicateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: duplicateID, Action: "reject", Reason: "duplicate regression"}); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Fatalf("rejected duplicate leaked older active claim: %#v", claims)
	}
}

func TestAcceptedDuplicateSingletonBeatsNewerActive(t *testing.T) {
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
	acceptedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "Run focused tests.",
		Confidence: 0.7,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedID, Action: "accept", Reason: "duplicate regression"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "Run focused tests.",
		Confidence: 0.99,
		SourceMode: "inferred",
	}); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].ID != acceptedID || claims[0].Status != "accepted" {
		t.Fatalf("accepted duplicate was not authoritative: %#v", claims)
	}
}

func TestResolveIntentAppliesPolicyAndPersistsSnapshot(t *testing.T) {
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
	normalID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: normalID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	privateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Do not touch release credentials.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: privateID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "Run every test.",
		Confidence: 0.5,
		SourceMode: "inferred",
	}); err != nil {
		t.Fatal(err)
	}
	first, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI: "tideglass://v1/intent/work.project.start/current",
		Task: IntentTask{
			Mode:     "act_gate",
			Autonomy: "bounded_act",
			Goal:     "start a project",
		},
		Contract: IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != "tideglass.intent_response.v2" || first.ResolvedURI != "tideglass://v1/profile/me/work.project.start/current" {
		t.Fatalf("bad envelope metadata: %#v", first)
	}
	if first.RequestID == "" || first.Resource.Kind != "work.project.start" || first.Decision.Autonomy != "bounded_act" {
		t.Fatalf("missing v2 response contract: %#v", first)
	}
	if first.ProfileHash == "" || !strings.HasPrefix(first.ProfileHash, "sha256:") || first.SnapshotID == "" {
		t.Fatalf("missing hash/snapshot: %#v", first)
	}
	if first.Commitments.ResponseHash != first.ProfileHash || first.Commitments.ClaimRoot == "" || first.Commitments.SnapshotID != first.SnapshotID {
		t.Fatalf("bad commitments: %#v", first.Commitments)
	}
	if _, ok := first.Links["self"]; ok {
		t.Fatalf("advertised unresolved self link: %#v", first.Links)
	}
	hashView := first
	hashView.RequestID = ""
	hashView.ProfileHash = ""
	hashView.SnapshotID = ""
	hashView.Commitments.ResponseHash = ""
	hashView.Commitments.SnapshotID = ""
	delete(hashView.Links, "self")
	hashJSON, err := json.Marshal(hashView)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(hashJSON), "profile_hash") || strings.Contains(string(hashJSON), "snapshot_id") || strings.Contains(string(hashJSON), "response_hash") {
		t.Fatalf("hash view includes mutable metadata: %s", hashJSON)
	}
	if got := "sha256:" + hashBytes(hashJSON); got != first.ProfileHash {
		t.Fatalf("recomputed profile hash = %s, want %s", got, first.ProfileHash)
	}
	if len(first.Claims) != 1 || first.Claims[0].ID != normalID {
		t.Fatalf("policy-filtered claims = %#v", first.Claims)
	}
	if first.Policy.MayAct || !first.Policy.NeedsUserAnswer {
		t.Fatalf("policy = %#v", first.Policy)
	}
	if !containsString(first.Policy.Redacted, "boundary.project.no_go") {
		t.Fatalf("redactions = %#v", first.Policy.Redacted)
	}
	second, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:  "tideglass://intent/work.project.start",
		Task: IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if second.ProfileHash != first.ProfileHash {
		t.Fatalf("profile hash changed for same materialized response: %s != %s", second.ProfileHash, first.ProfileHash)
	}
	if second.SnapshotID == first.SnapshotID {
		t.Fatalf("snapshot id should be unique per resolution: %s", second.SnapshotID)
	}
	var requests, snapshots int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from intent_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := tg.db.QueryRowContext(ctx, `select count(*) from profile_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || snapshots != 2 {
		t.Fatalf("requests=%d snapshots=%d", requests, snapshots)
	}
	var requestJSON string
	if err := tg.db.QueryRowContext(ctx, `select request_json from intent_requests where id = ?`, first.RequestID).Scan(&requestJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestJSON, "required_slots") || !strings.Contains(requestJSON, "bounded_act") || !strings.Contains(requestJSON, `"allow_action":true`) {
		t.Fatalf("request_json did not preserve v2 envelope: %s", requestJSON)
	}
	var snapshotJSON string
	if err := tg.db.QueryRowContext(ctx, `select response_json from profile_snapshots where id = ?`, first.SnapshotID).Scan(&snapshotJSON); err != nil {
		t.Fatal(err)
	}
	var stored IntentResponseEnvelope
	if err := json.Unmarshal([]byte(snapshotJSON), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.SnapshotID != first.SnapshotID || stored.Commitments.SnapshotID != first.SnapshotID {
		t.Fatalf("stored snapshot differs from returned envelope: stored=%#v returned=%#v", stored.Commitments, first.Commitments)
	}
	if _, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, NoPersist: true, Request: IntentRequestEnvelope{
		URI:  "tideglass://v1/intent/work.project.start/current",
		Task: IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
	}}); err == nil {
		t.Fatal("expected action authorization without persistence to fail")
	}
}

func TestResolveIntentSupportsDisclosureAndMissingIntent(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	response, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: "tideglass://disclosure/social.dinner/venue"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Intent.Kind != "social.dinner" || response.Policy.Audience != "venue" {
		t.Fatalf("response = %#v", response)
	}
	if response.Status != "missing" || !response.Policy.NeedsUserAnswer || response.Policy.MayAct {
		t.Fatalf("missing intent policy = %#v", response.Policy)
	}
	if len(response.Unresolved) == 0 || response.Unresolved[0].Kind != "preference.food.allergy" {
		t.Fatalf("unresolved = %#v", response.Unresolved)
	}
}

func TestResolveIntentActionGateProcessesScopedCriticalClaims(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.food.allergy",
		Value:      "Shellfish allergy.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/social.dinner/current",
		Task:     IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Audience: IntentAudience{Type: "service", ID: "venue"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer {
		t.Fatalf("scoped critical claim authorized action: %#v", response)
	}
	if !containsString(response.Policy.Redacted, "preference.food.allergy") || !hasBlockingQuestionSlot(response.Unresolved, "preference.food.allergy") {
		t.Fatalf("scoped critical claim was not processed as a blocker: %#v", response)
	}
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.food.allergy",
		Value:      "Shellfish allergy.",
		Confidence: 0.95,
		SourceMode: "explicit",
	}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/social.dinner/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || len(response.Claims) != 1 {
		t.Fatalf("bounded action was not held for confirmation with accepted identical constraint: %#v", response)
	}
}

func TestResolveIntentMinimalDisclosureMarksOmittedCriticalClaims(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.food.allergy",
		Value:      "Shellfish allergy.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/disclosure/social.dinner/venue",
		Disclosure: IntentDisclosure{Mode: "minimal"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status == "ready" || !response.Decision.NeedsUserAnswer || response.Policy.MayAct {
		t.Fatalf("minimal disclosure silently omitted critical claim: %#v", response)
	}
	if !containsString(response.Policy.Redacted, "preference.food.allergy") || !hasBlockingQuestionSlot(response.Unresolved, "preference.food.allergy") {
		t.Fatalf("omitted critical claim was not marked unresolved/redacted: %#v", response)
	}
}

func TestResolveIntentFullDisclosureScopesExternalAudiences(t *testing.T) {
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
	communicationID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: communicationID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	validationID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "Run every test.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: validationID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Audience:   IntentAudience{Type: "service", ID: "external-tool"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Disclosure: IntentDisclosure{Mode: "full", AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 1 || response.Claims[0].ID != communicationID {
		t.Fatalf("external full disclosure leaked unrelated claims: %#v", response.Claims)
	}
	if hasIntentClaimID(response.Claims, validationID) {
		t.Fatalf("external full disclosure leaked validation claim: %#v", response.Claims)
	}
}

func TestResolveIntentActionGateUsesLosslessDuplicateKeys(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	allergyID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "preference.food.allergy", Value: "No allergies.", Confidence: 0.95, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: allergyID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	topicID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "boundary.social.topic", Value: "External publishing", Confidence: 0.95, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: topicID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "boundary.social.topic", Value: "No external publishing", Confidence: 0.95, SourceMode: "explicit"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/social.dinner/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "boundary.social.topic") {
		t.Fatalf("lossy duplicate key suppressed distinct pending boundary: %#v", response)
	}
}

func TestResolveIntentActionGateSnapshotsLosslessAcceptedBoundaries(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	allergyID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "preference.food.allergy", Value: "No allergies.", Confidence: 0.95, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: allergyID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	noTopicID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "boundary.social.topic", Value: "No external publishing", Confidence: 0.95, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: noTopicID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	yesTopicID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "boundary.social.topic", Value: "External publishing", Confidence: 0.95, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: yesTopicID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	request := normalizeIntentRequest(IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/social.dinner/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "book dinner", Deadline: deadline},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	})
	constraintJSON, err := json.Marshal(map[string]any{
		"allow":         true,
		"intent_kind":   "social.dinner",
		"task_mode":     "act_gate",
		"autonomy":      "bounded_act",
		"goal":          "book dinner",
		"audience_type": "agent",
		"deadline":      deadline,
		"scope_hash":    actionScopeHash(request),
	})
	if err != nil {
		t.Fatal(err)
	}
	constraintID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "policy.action.constraints",
		Value:      string(constraintJSON),
		Confidence: 0.99,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: constraintID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: request})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Decision.MayAct || response.Decision.NeedsUserAnswer || response.Status != "ready" {
		t.Fatalf("lossless accepted boundaries blocked authorization: %#v", response)
	}
	foundNoTopic := false
	for _, claim := range response.Claims {
		if claim.ID == noTopicID {
			foundNoTopic = true
		}
	}
	if !foundNoTopic {
		t.Fatalf("authorized response omitted lossless accepted boundary: %#v", response.Claims)
	}
}

func TestResolveIntentActionGateAuthorizesWithPolicyConstraint(t *testing.T) {
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
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	baseActionRequest := normalizeIntentRequest(IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "start the project", Deadline: deadline},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	})
	constraintValue := func(allow bool, goal string) string {
		t.Helper()
		request := baseActionRequest
		request.Task.Goal = goal
		data, err := json.Marshal(map[string]any{
			"allow":         allow,
			"intent_kind":   "work.project.start",
			"task_mode":     "act_gate",
			"autonomy":      "bounded_act",
			"goal":          goal,
			"audience_type": "agent",
			"deadline":      deadline,
			"scope_hash":    actionScopeHash(request),
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	constraintID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "policy.action.constraints",
		Value:      constraintValue(false, "start the project"),
		Confidence: 0.99,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: constraintID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "start the project", Deadline: deadline},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "policy.action.constraints") {
		t.Fatalf("denying action constraint authorized bounded action: %#v", response)
	}
	constraintID, err = tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "policy.action.constraints",
		Value:      constraintValue(true, "start the project"),
		Confidence: 0.99,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: constraintID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "policy.action.constraints",
		Value:      constraintValue(true, "start another project"),
		Confidence: 0.99,
		SourceMode: "explicit",
	}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "start the project", Deadline: deadline},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Decision.MayAct || response.Decision.NeedsUserAnswer || response.Decision.Reason != "ready" || response.Status != "ready" {
		t.Fatalf("explicit action constraint did not authorize bounded action: %#v", response)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "start the project", Deadline: deadline},
		Audience:   IntentAudience{Type: "agent", ShareWith: []string{"external-service"}},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "policy.action.constraints") {
		t.Fatalf("unbound share_with target authorized bounded action: %#v", response)
	}
	constraintID, err = tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "policy.action.constraints",
		Value:      constraintValue(false, "start the project"),
		Confidence: 0.99,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: constraintID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "start the project", Deadline: deadline},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "policy.action.constraints") {
		t.Fatalf("newer denying action constraint did not revoke bounded action: %#v", response)
	}
}

func TestActionConstraintRejectsMalformedOrExpiredValues(t *testing.T) {
	deadline := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	request := normalizeIntentRequest(IntentRequestEnvelope{
		URI:  "tideglass://v1/intent/work.project.start/current",
		Task: IntentTask{Mode: "act_gate", Autonomy: "bounded_act", Goal: "start the project", Deadline: deadline},
	})
	value := func(deadline string, request IntentRequestEnvelope) string {
		t.Helper()
		data, err := json.Marshal(map[string]any{
			"allow":         true,
			"intent_kind":   "work.project.start",
			"task_mode":     "act_gate",
			"autonomy":      "bounded_act",
			"goal":          "start the project",
			"audience_type": "agent",
			"deadline":      deadline,
			"scope_hash":    actionScopeHash(request),
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if _, ok := matchingActionConstraint("work.project.start", ClaimOut{Kind: "policy.action.constraints", Value: value(deadline, request) + `{}`}, request); ok {
		t.Fatal("trailing JSON authorized action constraint")
	}
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	request.Task.Deadline = expired
	if _, ok := matchingActionConstraint("work.project.start", ClaimOut{Kind: "policy.action.constraints", Value: value(expired, request)}, request); ok {
		t.Fatal("expired deadline authorized action constraint")
	}
}

func TestResolveIntentMigratesLegacyEditedAcceptedClaims(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `delete from maintenance_tasks where name = 'legacy_reviewed_edit_reconciliation_v1'`); err != nil {
		t.Fatal(err)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "preference.food.allergy", Value: "No allergies.", Confidence: 0.95, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	var reviewCreatedAt string
	if err := tg.db.QueryRowContext(ctx, `select created_at from edits where claim_id = ? and operation = 'accept' order by rowid desc limit 1`, claimID).Scan(&reviewCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
		id("edt"), claimID, "supersede", `{"value":"Shellfish allergy."}`, "legacy edit", reviewCreatedAt); err != nil {
		t.Fatal(err)
	}
	rejectedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{Kind: "boundary.social.topic", Value: "Old rejected topic.", Confidence: 0.8, SourceMode: "explicit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: rejectedID, Action: "reject", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	var rejectCreatedAt string
	if err := tg.db.QueryRowContext(ctx, `select created_at from edits where claim_id = ? and operation = 'reject' order by rowid desc limit 1`, rejectedID).Scan(&rejectCreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
		id("edt"), rejectedID, "supersede", `{"value":"Fresh rejected edit."}`, "legacy edit", rejectCreatedAt); err != nil {
		t.Fatal(err)
	}
	if err := tg.ensureIntentRequestSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := tg.db.QueryRowContext(ctx, `select status from claims where id = ?`, rejectedID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("legacy rejected edited claim status = %s, want active", status)
	}
	var markerCount int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from maintenance_tasks where name = 'legacy_reviewed_edit_reconciliation_v1'`).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatalf("legacy reconciliation marker count = %d, want 1", markerCount)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set status = 'accepted' where id = ?`, rejectedID); err != nil {
		t.Fatal(err)
	}
	if err := tg.ensureIntentRequestSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tg.db.QueryRowContext(ctx, `select status from claims where id = ?`, rejectedID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "accepted" {
		t.Fatalf("legacy reconciliation reran after marker, status = %s", status)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/social.dinner/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "preference.food.allergy") {
		t.Fatalf("legacy edited accepted claim authorized action: %#v", response)
	}
}

func TestResolveIntentActionGateFailsClosedWithoutConstraints(t *testing.T) {
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
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:  "tideglass://v1/intent/work.project.start/current",
		Task: IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "policy.action.constraints") {
		t.Fatalf("empty action gate authorized action: %#v", response)
	}
	claimID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:  "tideglass://v1/intent/work.project.start/current",
		Task: IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "policy.action.constraints") {
		t.Fatalf("unrelated accepted claim authorized action: %#v", response)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Task:     IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract: IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "policy.action.constraints") {
		t.Fatalf("caller-selected benign required slot authorized action: %#v", response)
	}
}

func TestResolveIntentActionGateScansPendingSingletonBoundaries(t *testing.T) {
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
	communicationID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: communicationID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	oldBoundaryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Old reviewed boundary.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: oldBoundaryID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	pendingBoundaryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "New pending boundary.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, err := tg.Profile(ctx, ProfileOptions{Kind: "work.project.start"})
	if err != nil {
		t.Fatal(err)
	}
	if hasClaimID(profile.Claims, pendingBoundaryID) {
		t.Fatalf("pending action boundary leaked into canonical profile: %#v", profile.Claims)
	}
	profile, err = tg.Profile(ctx, ProfileOptions{Kind: "work.project.start", ReviewCandidates: true})
	if err != nil {
		t.Fatal(err)
	}
	if !hasClaimID(profile.Claims, pendingBoundaryID) {
		t.Fatalf("pending action boundary was hidden from review profile: %#v", profile.Claims)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("pending singleton boundary was hidden by accepted boundary: %#v", response)
	}
}

func TestResolveIntentActionGateIgnoresSupersededSingletonBoundaries(t *testing.T) {
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
	communicationID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: communicationID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	oldBoundaryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "New reviewed boundary.",
		Confidence: 0.8,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set updated_at = ?, revision = 0 where id = ?`, "2024-01-01T00:00:00Z", oldBoundaryID); err != nil {
		t.Fatal(err)
	}
	newBoundaryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "New reviewed boundary.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: newBoundaryID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || hasBlockingQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("superseded singleton boundary blocked action: %#v", response)
	}
	if _, err := tg.EditClaim(ctx, EditOptions{ClaimID: oldBoundaryID, Value: "Fresh pending boundary.", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("fresh edited singleton boundary did not block action: %#v", response)
	}
}

func TestLoadActionGateClaimsTreatsRevisionZeroAcceptedSingletonAsAuthoritative(t *testing.T) {
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
	acceptedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Ask before touching production.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	activeID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Old pending boundary.",
		Confidence: 0.7,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id = ?`, "2024-01-02T00:00:00Z", acceptedID); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id = ?`, "2024-01-01T00:00:00Z", activeID); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadActionGateClaims(ctx, intent.ID, "work.project.start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasClaimID(claims, activeID) {
		t.Fatalf("revision-zero accepted singleton did not suppress active claim: %#v", claims)
	}
	if !hasClaimID(claims, acceptedID) {
		t.Fatalf("revision-zero accepted singleton missing from action gate claims: %#v", claims)
	}
}

func TestLoadActionGateClaimsKeepsNewerRevisionZeroPendingSingleton(t *testing.T) {
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
	acceptedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Old accepted boundary.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	pendingID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "New pending boundary.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id = ?`, "2024-01-01T00:00:00Z", acceptedID); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id = ?`, "2024-01-02T00:00:00Z", pendingID); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadActionGateClaims(ctx, intent.ID, "work.project.start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasClaimID(claims, pendingID) {
		t.Fatalf("newer revision-zero pending singleton was hidden: %#v", claims)
	}
}

func TestLoadActionGateClaimsKeepsTiedRevisionZeroPendingSingleton(t *testing.T) {
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
	acceptedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Accepted boundary.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	pendingID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Ambiguous pending boundary.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id in (?, ?)`, "2024-01-01T00:00:00Z", acceptedID, pendingID); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadActionGateClaims(ctx, intent.ID, "work.project.start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasClaimID(claims, pendingID) {
		t.Fatalf("tied revision-zero pending singleton was hidden: %#v", claims)
	}
}

func TestLoadActionGateClaimsDropsSupersededAcceptedSingletons(t *testing.T) {
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
	oldID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Old accepted boundary.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: oldID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	newID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "New accepted boundary.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: newID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadActionGateClaims(ctx, intent.ID, "work.project.start", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hasClaimID(claims, oldID) {
		t.Fatalf("superseded accepted singleton stayed in action gate claims: %#v", claims)
	}
	if !hasClaimID(claims, newID) {
		t.Fatalf("new accepted singleton missing from action gate claims: %#v", claims)
	}
}

func TestLoadActionGateClaimsDropsTiedAcceptedDuplicates(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.social.topic",
		Value:      "No external publishing.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: firstID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	secondID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.social.topic",
		Value:      "No external publishing.",
		Confidence: 0.95,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: secondID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id in (?, ?)`, "2024-01-01T00:00:00Z", firstID, secondID); err != nil {
		t.Fatal(err)
	}
	claims, err := tg.loadActionGateClaims(ctx, intent.ID, "social.dinner", nil)
	if err != nil {
		t.Fatal(err)
	}
	duplicateCount := 0
	for _, claim := range claims {
		if claim.Kind == "boundary.social.topic" && claim.Value == "No external publishing." {
			duplicateCount++
		}
	}
	if duplicateCount != 1 {
		t.Fatalf("accepted duplicate count = %d, claims = %#v", duplicateCount, claims)
	}
}

func TestLoadReviewCandidateClaimsPreservesDuplicateDecisions(t *testing.T) {
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
	acceptedDuplicateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedDuplicateID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	activeDuplicateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.communication",
		Value:      "Use terse updates.",
		Confidence: 0.95,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejectedSuppressedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Never deploy on Fridays.",
		Confidence: 0.7,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejectedDuplicateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Never deploy on Fridays.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: rejectedDuplicateID, Action: "reject", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	acceptedBoundaryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "No external publishing.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedBoundaryID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	losslessCandidateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "External publishing.",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set revision = 0, updated_at = ? where id in (?, ?)`, "2024-01-01T00:00:00Z", acceptedBoundaryID, losslessCandidateID); err != nil {
		t.Fatal(err)
	}
	acceptedSingletonID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "Run the full suite.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: acceptedSingletonID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	freshCandidateID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.project.validation",
		Value:      "Run focused tests first.",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := tg.loadReviewCandidateClaims(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hasClaimID(candidates, activeDuplicateID) {
		t.Fatalf("accepted duplicate was shown for review: %#v", candidates)
	}
	if hasClaimID(candidates, rejectedSuppressedID) {
		t.Fatalf("rejected duplicate resurrected active claim: %#v", candidates)
	}
	if !hasClaimID(candidates, freshCandidateID) {
		t.Fatalf("fresh singleton candidate was hidden: %#v", candidates)
	}
	if !hasClaimID(candidates, losslessCandidateID) {
		t.Fatalf("losslessly distinct singleton candidate was hidden: %#v", candidates)
	}
}

func TestResolveIntentInferredClaimsDoNotSatisfyCriticalQuestions(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.food.allergy",
		Value:      "Maybe shellfish.",
		Confidence: 0.6,
		SourceMode: "inferred",
	}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:       "tideglass://v1/intent/social.dinner/current",
		Freshness: IntentFreshness{AcceptInferredForQuestions: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "partial" || !response.Policy.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "preference.food.allergy") {
		t.Fatalf("inferred critical claim satisfied critical question: %#v", response)
	}
}

func TestResolveIntentUnreviewedAndStaleClaimsDoNotSatisfyCriticalSlots(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	intent, err := tg.ensureIntent(ctx, "social.dinner", "Dinner with strangers")
	if err != nil {
		t.Fatal(err)
	}
	unreviewedID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.food.allergy",
		Value:      "No shellfish.",
		Confidence: 0.8,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: "tideglass://intent/social.dinner"}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Policy.NeedsUserAnswer || response.Policy.MayAct {
		t.Fatalf("unreviewed critical claim satisfied policy: %#v", response.Policy)
	}
	if len(response.Claims) != 0 {
		t.Fatalf("unreviewed claim leaked into default response: %#v", response.Claims)
	}
	requireReviewed := false
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:       "tideglass://intent/social.dinner",
		Task:      IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Freshness: IntentFreshness{RequireReviewed: &requireReviewed},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Policy.NeedsUserAnswer || response.Policy.MayAct || !hasQuestionSlot(response.Unresolved, "preference.food.allergy") {
		t.Fatalf("unreviewed critical claim satisfied action slot: %#v", response)
	}
	if response.Policy.Freshness != "unreviewed_allowed" {
		t.Fatalf("unreviewed freshness was mislabeled: %#v", response.Policy)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: unreviewedID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: "tideglass://intent/social.dinner"}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Policy.NeedsUserAnswer || response.Policy.MayAct || !containsString(response.Policy.Redacted, "preference.food.allergy") {
		t.Fatalf("redacted critical claim authorized action: %#v", response.Policy)
	}
	if !hasQuestionSlot(response.Unresolved, "preference.food.allergy") {
		t.Fatalf("redacted critical claim did not produce an unresolved slot: %#v", response.Unresolved)
	}
	old := "2020-01-01T00:00:00Z"
	if _, err := tg.db.ExecContext(ctx, `update claims set updated_at = ? where id = ?`, old, unreviewedID); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.EditClaim(ctx, EditOptions{ClaimID: unreviewedID, Value: "No shellfish or peanuts.", Reason: "fresh edit"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://intent/social.dinner",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Freshness:  IntentFreshness{MaxAge: "1h"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Policy.NeedsUserAnswer || response.Policy.MayAct {
		t.Fatalf("fresh edit was authorized before re-review: response=%#v", response)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: unreviewedID, Action: "accept", Reason: "review edited value"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://intent/social.dinner",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Freshness:  IntentFreshness{MaxAge: "1h"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Policy.NeedsUserAnswer || response.Policy.MayAct || len(response.Claims) != 1 || response.Claims[0].Value != "No shellfish or peanuts." {
		t.Fatalf("reviewed fresh edit was treated as stale: response=%#v", response)
	}
	staleOnlyID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.food.dietary_restriction",
		Value:      "Vegetarian.",
		Confidence: 0.8,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: staleOnlyID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.db.ExecContext(ctx, `update claims set updated_at = ? where id = ?`, old, staleOnlyID); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://intent/social.dinner",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Freshness:  IntentFreshness{MaxAge: "1h"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range response.Claims {
		if claim.ID == staleOnlyID {
			t.Fatalf("stale claim leaked through max_age: %#v", response.Claims)
		}
	}
}

func TestResolveIntentV2ActionAndDisclosureContracts(t *testing.T) {
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
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	rawMemoryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "preference.agent.imported_memory",
		Value:      "raw private exported memory",
		Confidence: 0.9,
		SourceMode: "inferred",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: rawMemoryID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Task:     IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract: IntentContract{RequiredSlots: []string{"boundary.project.no_go"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("required built-in boundary slot did not block action: %#v", response)
	}
	boundaryID, err := tg.insertClaim(ctx, intent.ID, candidateClaim{
		Kind:       "boundary.project.no_go",
		Value:      "Do not publish externally without explicit review.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.project.validation"}},
		Disclosure: IntentDisclosure{Mode: "full"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasQuestionSlot(response.Unresolved, "preference.project.validation") {
		t.Fatalf("required slot did not gate action: %#v", response)
	}
	if !containsString(response.Policy.Redacted, "preference.agent.imported_memory") {
		t.Fatalf("imported memory was not redacted: %#v", response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Task:     IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract: IntentContract{RequiredSlots: []string{"preference.agent.communication"}, ConfidenceFloor: 0.95},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !hasQuestionSlot(response.Unresolved, "preference.agent.communication") {
		t.Fatalf("low-confidence claim satisfied required slot: %#v", response)
	}
	if len(response.Claims) != 0 {
		t.Fatalf("low-confidence claim leaked through response floor: %#v", response.Claims)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Task:     IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract: IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("unreviewed boundary claim authorized action: %#v", response)
	}
	requireReviewed := false
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Freshness:  IntentFreshness{RequireReviewed: &requireReviewed},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer || !hasBlockingQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("unreviewed boundary claim bypassed action with require_reviewed=false: %#v", response)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "context", Autonomy: "context_only"},
		Freshness:  IntentFreshness{RequireReviewed: &requireReviewed},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "ready" || response.Policy.NeedsUserAnswer || hasBlockingQuestionSlot(response.Unresolved, "boundary.project.no_go") {
		t.Fatalf("context resolve emitted action-gate blockers: %#v", response)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: boundaryID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Audience: IntentAudience{Type: "service", ID: "venue"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 {
		t.Fatalf("external minimal response leaked unrelated claims: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Audience: IntentAudience{Type: "service", ID: "local"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 {
		t.Fatalf("reserved external audience id leaked unrelated claims: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Audience: IntentAudience{Type: "agent", ID: "external-service"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 {
		t.Fatalf("external agent audience id leaked unrelated claims: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Audience: IntentAudience{Type: "agent", ShareWith: []string{"venue"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 {
		t.Fatalf("share_with external target leaked unrelated claims: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Actor:    IntentActor{Type: "agent", ID: "venue"},
		Audience: IntentAudience{Type: "agent", ShareWith: []string{"venue"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 {
		t.Fatalf("actor spoof leaked unrelated claims: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://disclosure/work.project.start/agent",
		Audience: IntentAudience{ShareWith: []string{"venue"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 {
		t.Fatalf("disclosure URI dropped external share target: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://disclosure/work.project.start/venue",
		Audience: IntentAudience{Type: "service", ID: "local"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 0 || len(response.Policy.SafeToShare) != 0 || response.Policy.Audience != "venue" {
		t.Fatalf("disclosure URI was not authoritative: claims=%#v policy=%#v", response.Claims, response.Policy)
	}
	requestContext := map[string]any{"trace": "original"}
	if _, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:     "tideglass://v1/intent/work.project.start/current",
		Context: requestContext,
	}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := requestContext["server_authority"]; ok {
		t.Fatalf("resolve mutated caller context: %#v", requestContext)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:      "tideglass://v1/intent/work.project.start/current",
		Audience: IntentAudience{Type: "service", ID: "venue"},
		Contract: IntentContract{OptionalSlots: []string{"preference.agent.communication"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 1 || response.Claims[0].Kind != "preference.agent.communication" {
		t.Fatalf("contract-scoped minimal response = %#v", response.Claims)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "suggest_then_confirm"},
		Disclosure: IntentDisclosure{AllowSensitive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "partial" || response.Decision.MayAct || response.Decision.Reason != "confirmation_required" {
		t.Fatalf("suggest_then_confirm authorized action: status=%s decision=%#v", response.Status, response.Decision)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:  "tideglass://v1/intent/work.project.start/current",
		Task: IntentTask{Mode: "context", Autonomy: "bounded_act"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct {
		t.Fatalf("non-act_gate mode authorized action: %#v", response.Decision)
	}
	allowValues := false
	allowCommitments := false
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Disclosure: IntentDisclosure{AllowValues: &allowValues},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer {
		t.Fatalf("value-less action gate authorized action: %#v", response)
	}
	allowValues = true
	response, err = tg.ResolveIntent(ctx, ResolveOptions{AllowAction: true, Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "act_gate", Autonomy: "bounded_act"},
		Contract:   IntentContract{RequiredSlots: []string{"preference.agent.communication"}},
		Disclosure: IntentDisclosure{Mode: "existence", AllowValues: &allowValues},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Decision.MayAct || !response.Decision.NeedsUserAnswer {
		t.Fatalf("existence action gate authorized action: %#v", response)
	}
	allowValues = false
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://v1/intent/work.project.start/current",
		Task:       IntentTask{Mode: "context", Autonomy: "context_only"},
		Disclosure: IntentDisclosure{AllowValues: &allowValues, AllowCommitments: &allowCommitments},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 1 || response.Claims[0].Value != "" {
		t.Fatalf("allow_values=false leaked values: %#v", response.Claims)
	}
	if response.Claims[0].Commitment != "" || response.Commitments.ResponseHash != "" || response.Commitments.ClaimRoot != "" {
		t.Fatalf("allow_commitments=false emitted commitments: claims=%#v commitments=%#v", response.Claims, response.Commitments)
	}
}

func TestResolveIntentCanonicalLinksAndExistenceDisclosure(t *testing.T) {
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
		Value:      "Use terse updates.",
		Confidence: 0.9,
		SourceMode: "explicit",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tg.ReviewClaim(ctx, ReviewOptions{ClaimID: claimID, Action: "accept", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	response, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: "tideglass://profile/me/work.project.start/current"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Intent.Kind != "work.project.start" {
		t.Fatalf("profile uri response = %#v", response)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: "tideglass://unresolved/work.project.start"}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Intent.Kind != "work.project.start" {
		t.Fatalf("unresolved uri response = %#v", response)
	}
	response, err = tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://intent/work.project.start",
		Disclosure: IntentDisclosure{Mode: "EXISTENCE"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Claims) != 1 {
		t.Fatalf("existence claims = %#v", response.Claims)
	}
	claim := response.Claims[0]
	if claim.Kind != "preference.agent.communication" || claim.Status != "accepted" {
		t.Fatalf("existence claim identity = %#v", claim)
	}
	if claim.ID != "" || claim.Value != "" || claim.Confidence != 0 || claim.SourceMode != "" || claim.Sensitivity != "" || len(claim.Evidence) != 0 {
		t.Fatalf("existence claim leaked metadata: %#v", claim)
	}
	if claim.Commitment != "" || response.Commitments.ResponseHash != "" {
		t.Fatalf("existence response emitted hidden-value commitments: claim=%#v commitments=%#v", claim, response.Commitments)
	}
	if _, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:        "tideglass://intent/work.project.start",
		Disclosure: IntentDisclosure{Mode: "verbose"},
	}}); err == nil {
		t.Fatal("expected unsupported disclosure mode to fail closed")
	}
	if _, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:  "tideglass://intent/work.project.start",
		Task: IntentTask{Mode: "review"},
	}}); err == nil {
		t.Fatal("expected unsupported review task mode to fail closed")
	}
	if _, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:       "tideglass://intent/work.project.start",
		Freshness: IntentFreshness{MaxAge: "-1h"},
	}}); err == nil {
		t.Fatal("expected negative freshness to fail closed")
	}
	if _, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{
		URI:       "tideglass://intent/work.project.start",
		Freshness: IntentFreshness{MaxAge: "106752d"},
	}}); err == nil {
		t.Fatal("expected overflowing freshness to fail closed")
	}
	for _, uri := range []string{"tideglass://intent/", "tideglass://unresolved/", "tideglass://profile/me//current", "tideglass://v1/v1/intent/work.project.start"} {
		if _, err := tg.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: uri}}); err == nil {
			t.Fatalf("expected empty kind URI to fail: %s", uri)
		}
	}
}

func TestServiceHandlerResolvesIntentResource(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	token := "test-token"
	server := httptest.NewServer(NewServiceHandlerWithToken(tg, token))
	defer server.Close()
	health, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", health.StatusCode)
	}
	var requestsBeforeResource int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from intent_requests`).Scan(&requestsBeforeResource); err != nil {
		t.Fatal(err)
	}
	unauthResource, err := http.Get(server.URL + "/resource?uri=tideglass://intent/work.project.start")
	if err != nil {
		t.Fatal(err)
	}
	defer unauthResource.Body.Close()
	if unauthResource.StatusCode != http.StatusUnauthorized {
		data, _ := io.ReadAll(unauthResource.Body)
		t.Fatalf("unauth resource status = %d body=%s", unauthResource.StatusCode, data)
	}
	resource, err := authedHTTP(t, http.MethodGet, server.URL+"/resource?uri=tideglass://intent/work.project.start", "", nil, token)
	if err != nil {
		t.Fatal(err)
	}
	defer resource.Body.Close()
	if resource.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resource.Body)
		t.Fatalf("resource status = %d body=%s", resource.StatusCode, data)
	}
	var response IntentResponseEnvelope
	if err := json.NewDecoder(resource.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Intent.Kind != "work.project.start" || response.ProfileHash == "" {
		t.Fatalf("response = %#v", response)
	}
	var requestsAfterResource int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from intent_requests`).Scan(&requestsAfterResource); err != nil {
		t.Fatal(err)
	}
	if requestsAfterResource != requestsBeforeResource {
		t.Fatalf("resource read persisted request: before=%d after=%d", requestsBeforeResource, requestsAfterResource)
	}
	csrf := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue"}`)
	csrfResponse, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "text/plain", csrf, token)
	defer csrfResponse.Body.Close()
	if csrfResponse.StatusCode != http.StatusForbidden {
		data, _ := io.ReadAll(csrfResponse.Body)
		t.Fatalf("text/plain resolve status = %d body=%s", csrfResponse.StatusCode, data)
	}
	body := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue"}`)
	resolve, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", body, token)
	defer resolve.Body.Close()
	if resolve.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(resolve.Body)
		t.Fatalf("resolve status = %d body=%s", resolve.StatusCode, data)
	}
	response = IntentResponseEnvelope{}
	if err := json.NewDecoder(resolve.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Policy.Audience != "venue" || response.ResolvedURI != "tideglass://v1/profile/me/social.dinner/current" {
		t.Fatalf("resolve response = %#v", response)
	}
	bigNumberBody := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue","context":{"trace":9007199254740993}}`)
	bigNumberResolve, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", bigNumberBody, token)
	if err != nil {
		t.Fatal(err)
	}
	defer bigNumberResolve.Body.Close()
	if bigNumberResolve.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(bigNumberResolve.Body)
		t.Fatalf("big-number resolve status = %d body=%s", bigNumberResolve.StatusCode, data)
	}
	response = IntentResponseEnvelope{}
	if err := json.NewDecoder(bigNumberResolve.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	var requestJSON string
	if err := tg.db.QueryRowContext(ctx, `select request_json from intent_requests where id = ?`, response.RequestID).Scan(&requestJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestJSON, "9007199254740993") || strings.Contains(requestJSON, "9007199254740992") {
		t.Fatalf("large HTTP context number was not preserved: %s", requestJSON)
	}
	unknownPolicyField := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue","disclosure":{"modee":"existence"}}`)
	unknownPolicyResponse, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", unknownPolicyField, token)
	if err != nil {
		t.Fatal(err)
	}
	defer unknownPolicyResponse.Body.Close()
	if unknownPolicyResponse.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(unknownPolicyResponse.Body)
		t.Fatalf("unknown policy field status = %d body=%s", unknownPolicyResponse.StatusCode, data)
	}
	unknownAudienceField := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue","audience":{"shareWith":["venue"]}}`)
	unknownAudienceResponse, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", unknownAudienceField, token)
	if err != nil {
		t.Fatal(err)
	}
	defer unknownAudienceResponse.Body.Close()
	if unknownAudienceResponse.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(unknownAudienceResponse.Body)
		t.Fatalf("unknown audience field status = %d body=%s", unknownAudienceResponse.StatusCode, data)
	}
	trailingResolve, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue"}{}`), token)
	if err != nil {
		t.Fatal(err)
	}
	defer trailingResolve.Body.Close()
	if trailingResolve.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(trailingResolve.Body)
		t.Fatalf("trailing resolve status = %d body=%s", trailingResolve.StatusCode, data)
	}
	oversizedResolve, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue","context":{"blob":"`+strings.Repeat("x", 1<<20)+`"}}`), token)
	if err != nil {
		t.Fatal(err)
	}
	defer oversizedResolve.Body.Close()
	if oversizedResolve.StatusCode != http.StatusRequestEntityTooLarge {
		data, _ := io.ReadAll(oversizedResolve.Body)
		t.Fatalf("oversized resolve status = %d body=%s", oversizedResolve.StatusCode, data)
	}
	unsupportedProof := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue","proof":{"zk_predicates":["age_over_18"]}}`)
	unsupportedProofResponse, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", unsupportedProof, token)
	if err != nil {
		t.Fatal(err)
	}
	defer unsupportedProofResponse.Body.Close()
	if unsupportedProofResponse.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(unsupportedProofResponse.Body)
		t.Fatalf("unsupported proof status = %d body=%s", unsupportedProofResponse.StatusCode, data)
	}
	unsupportedProofHash := strings.NewReader(`{"uri":"tideglass://disclosure/social.dinner/venue","proof":{"hash":"merkle"}}`)
	unsupportedProofHashResponse, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", unsupportedProofHash, token)
	if err != nil {
		t.Fatal(err)
	}
	defer unsupportedProofHashResponse.Body.Close()
	if unsupportedProofHashResponse.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(unsupportedProofHashResponse.Body)
		t.Fatalf("unsupported proof hash status = %d body=%s", unsupportedProofHashResponse.StatusCode, data)
	}
	bad, err := authedHTTP(t, http.MethodGet, server.URL+"/resource?uri=tideglass://intent/", "", nil, token)
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(bad.Body)
		t.Fatalf("bad resource status = %d body=%s", bad.StatusCode, data)
	}
	sensitive := strings.NewReader(`{"uri":"tideglass://intent/work.project.start","disclosure":{"allow_sensitive":true}}`)
	forbidden, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", sensitive, token)
	defer forbidden.Body.Close()
	if forbidden.StatusCode != http.StatusForbidden {
		data, _ := io.ReadAll(forbidden.Body)
		t.Fatalf("sensitive resolve status = %d body=%s", forbidden.StatusCode, data)
	}
	invalidTask := strings.NewReader(`{"uri":"tideglass://intent/work.project.start","task":{"mode":"ship_it"}}`)
	invalidResponse, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", invalidTask, token)
	defer invalidResponse.Body.Close()
	if invalidResponse.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(invalidResponse.Body)
		t.Fatalf("invalid task status = %d body=%s", invalidResponse.StatusCode, data)
	}
	req, err := http.NewRequest(http.MethodGet, server.URL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example"
	hostResponse, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer hostResponse.Body.Close()
	if hostResponse.StatusCode != http.StatusForbidden {
		data, _ := io.ReadAll(hostResponse.Body)
		t.Fatalf("evil host status = %d body=%s", hostResponse.StatusCode, data)
	}
}

func TestHandleMCPOnceReadsIntentResource(t *testing.T) {
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer tg.Close()
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"tideglass://intent/work.project.start"}}`)
	var output strings.Builder
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"jsonrpc": "2.0"`) || !strings.Contains(output.String(), `tideglass.intent_response.v2`) {
		t.Fatalf("mcp output = %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":"tool-1","method":"tools/call","params":{"name":"tideglass.resolve_intent","arguments":{"schema_version":"tideglass.intent_request.v2","uri":"tideglass://v1/intent/work.project.start/current","audience":{"type":"agent","id":"codex"},"task":{"mode":"context","autonomy":"suggest_only"}}}}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id": "tool-1"`) || !strings.Contains(output.String(), `tideglass.intent_response.v2`) || !strings.Contains(output.String(), `codex`) {
		t.Fatalf("mcp tool output = %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":"tool-2","method":"tools/call","params":{"name":"tideglass.resolve_intent","arguments":{"uri":"tideglass://v1/intent/work.project.start/current","disclosure":{"allow_sensitive":true}}}}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"error"`) || !strings.Contains(output.String(), `trusted server capability`) {
		t.Fatalf("expected MCP sensitive disclosure error response: %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":"tool-3","method":"tools/call","params":{"name":"tideglass.resolve_intent","arguments":{"uri":"tideglass://v1/intent/work.project.start/current","actor":{"capabilities":["read_sensitive"]},"disclosure":{"allow_sensitive":true}}}}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"error"`) || !strings.Contains(output.String(), `trusted server capability`) {
		t.Fatalf("expected self-declared MCP capability error response: %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":"tool-big","method":"tools/call","params":{"name":"tideglass.resolve_intent","arguments":{"uri":"tideglass://v1/intent/work.project.start/current","context":{"trace":9007199254740993}}}}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	var requestJSON string
	if err := tg.db.QueryRowContext(ctx, `select request_json from intent_requests order by rowid desc limit 1`).Scan(&requestJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(requestJSON, "9007199254740993") || strings.Contains(requestJSON, "9007199254740992") {
		t.Fatalf("large MCP context number was not preserved: %s", requestJSON)
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":"bad-uri","method":"resources/read","params":{"uri":"tideglass://intent/"}}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id": "bad-uri"`) || !strings.Contains(output.String(), `"error"`) {
		t.Fatalf("expected correlated MCP resource error response: %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":9007199254740993,"method":"resources/read","params":{"uri":"tideglass://intent/"}}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id": 9007199254740993`) || !strings.Contains(output.String(), `"error"`) {
		t.Fatalf("expected exact numeric MCP id in error response: %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0",`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id": null`) || !strings.Contains(output.String(), `"code": -32700`) {
		t.Fatalf("expected MCP parse error envelope: %s", output.String())
	}
	input = strings.NewReader(`{"jsonrpc":"2.0","id":"extra","method":"resources/read","params":{"uri":"tideglass://intent/work.project.start"}}{}`)
	output.Reset()
	if err := HandleMCPOnce(ctx, tg, input, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"id": null`) || !strings.Contains(output.String(), `"code": -32700`) {
		t.Fatalf("expected MCP trailing input parse error envelope: %s", output.String())
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

func TestProbeSourceRequiresRetrievalFTSTable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "slacrawl.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`create table messages(id integer primary key)`,
		`create table channels(id integer primary key)`,
		`create table users(id integer primary key)`,
		`insert into messages(id) values(1)`,
		`insert into channels(id) values(1)`,
		`insert into users(id) values(1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	status := probeSource(ctx, SourceStatus{ID: "slacrawl", Kind: "crawl", Locator: dbPath})
	if status.Health != "partial" || !strings.Contains(status.LastError, "message_fts") {
		t.Fatalf("status without fts = %#v", status)
	}
	if _, err := db.ExecContext(ctx, `create table message_fts(content text, message_key text)`); err != nil {
		t.Fatal(err)
	}
	status = probeSource(ctx, SourceStatus{ID: "slacrawl", Kind: "crawl", Locator: dbPath})
	if status.Health != "ok" || status.LastError != "" {
		t.Fatalf("status with fts = %#v", status)
	}
}

func TestProbeDiscrawlHealthReflectsEmbeddingAvailability(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "discrawl.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`create table messages(id integer primary key)`,
		`create table members(id integer primary key)`,
		`create table message_embeddings(id integer primary key)`,
		`create table message_fts(content text, message_id text)`,
		`insert into messages(id) values(1)`,
		`insert into members(id) values(1)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	status := probeSource(ctx, SourceStatus{ID: "discrawl", Kind: "crawl", Locator: dbPath})
	if status.Health != "partial" || !strings.Contains(status.LastError, "no message embeddings") {
		t.Fatalf("status without embeddings = %#v", status)
	}
	if _, err := db.ExecContext(ctx, `insert into message_embeddings(id) values(1)`); err != nil {
		t.Fatal(err)
	}
	status = probeSource(ctx, SourceStatus{ID: "discrawl", Kind: "crawl", Locator: dbPath, LastError: "old"})
	if status.Health != "ok" || status.LastError != "" {
		t.Fatalf("status with embeddings = %#v", status)
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

func TestPrintSanitizesHumanPathOutput(t *testing.T) {
	output := captureStdout(t, func() {
		if err := Print(SourceList{Sources: []SourceStatus{{
			ID:      "slacrawl",
			Kind:    "crawl",
			Health:  "ok",
			Locator: "/tmp/bad\x1b]52;c;clipboard\a.db",
		}}}, false); err != nil {
			t.Fatal(err)
		}
		if err := Print(ExportResult{Path: "/tmp/export\x1b[31m.tgz", Records: 1}, false); err != nil {
			t.Fatal(err)
		}
		if err := Print(DoctorResult{OverallState: "ok", DBPath: "/tmp/tide\x1b[31m.db", Schema: 1}, false); err != nil {
			t.Fatal(err)
		}
	})
	if strings.ContainsAny(output, "\x1b\a") {
		t.Fatalf("unsafe terminal path output preserved: %q", output)
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

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	originalStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = originalStdout
	}()
	fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func containsString(rows []string, value string) bool {
	for _, row := range rows {
		if row == value {
			return true
		}
	}
	return false
}

func authedHTTP(t *testing.T, method, url, contentType string, body io.Reader, token string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return http.DefaultClient.Do(req)
}

func hasQuestionSlot(rows []IntentQuestion, value string) bool {
	for _, row := range rows {
		slot := row.Slot
		if slot == "" {
			slot = row.Kind
		}
		if slot == value {
			return true
		}
	}
	return false
}

func hasClaimID(rows []ClaimOut, id string) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}

func hasIntentClaimID(rows []IntentClaimEnvelope, id string) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}

func hasBlockingQuestionSlot(rows []IntentQuestion, value string) bool {
	for _, row := range rows {
		slot := row.Slot
		if slot == "" {
			slot = row.Kind
		}
		if slot == value && (row.Priority == "critical" || row.BlocksAction) {
			return true
		}
	}
	return false
}
