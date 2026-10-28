package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTestTideglass(t *testing.T) (*Tideglass, context.Context) {
	t.Helper()
	ctx := context.Background()
	tg, err := Open(ctx, filepath.Join(t.TempDir(), "tideglass.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tg.Close() })
	return tg, ctx
}

// isolateSources keeps source probing away from the developer's real crawl
// databases and crawlbar install.
func isolateSources(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TIDEGLASS_CRAWLBAR", filepath.Join(t.TempDir(), "no-crawlbar"))
	t.Setenv("CRAWLBAR_BIN", "")
}

func TestFailClosedActionSnapshotRevokesActionAuthority(t *testing.T) {
	response := IntentResponseEnvelope{
		Status:      "ok",
		SnapshotID:  "snp_inconsistent",
		Policy:      IntentPolicyEnvelope{MayAct: true, CapabilityRequired: []string{"existing"}},
		Decision:    IntentDecision{MayAct: true, Reason: "authorized"},
		Commitments: IntentCommitments{SnapshotID: "snp_inconsistent", Algorithm: "sha256"},
	}
	got := failClosedActionSnapshot(response)
	if got.Policy.MayAct || got.Decision.MayAct {
		t.Fatalf("fail-closed snapshot still allows action: policy=%v decision=%v", got.Policy.MayAct, got.Decision.MayAct)
	}
	if !got.Policy.NeedsUserAnswer || !got.Decision.NeedsUserAnswer {
		t.Fatal("fail-closed snapshot must ask the user")
	}
	if got.Decision.Reason != "critical_slots_missing" || got.Status != "partial" {
		t.Fatalf("reason=%q status=%q", got.Decision.Reason, got.Status)
	}
	if got.SnapshotID != "" || got.Commitments.SnapshotID != "" {
		t.Fatalf("snapshot ids must be cleared: %q %q", got.SnapshotID, got.Commitments.SnapshotID)
	}
	if !hasQuestionSlot(got.Unresolved, "policy.action.snapshot") {
		t.Fatalf("unresolved = %#v, want policy.action.snapshot", got.Unresolved)
	}
	for _, question := range got.Unresolved {
		if questionSlot(question) == "policy.action.snapshot" && (!question.BlocksAction || question.Priority != "critical") {
			t.Fatalf("snapshot question must be critical and blocking: %#v", question)
		}
	}
	if !containsString(got.Policy.CapabilityRequired, "consistent_snapshot") || !containsString(got.Policy.CapabilityRequired, "existing") {
		t.Fatalf("capability_required = %#v", got.Policy.CapabilityRequired)
	}
	if got.ProfileHash == "" || got.ProfileHash != profileHash(got) {
		t.Fatalf("profile hash %q was not recomputed", got.ProfileHash)
	}
	if got.Commitments.ResponseHash != got.ProfileHash {
		t.Fatalf("commitment response hash %q != profile hash %q", got.Commitments.ResponseHash, got.ProfileHash)
	}
	if response.Policy.MayAct != true || response.SnapshotID != "snp_inconsistent" {
		t.Fatal("input response was mutated")
	}
}

func TestFailClosedActionSnapshotWithoutCommitmentsLeavesThemEmpty(t *testing.T) {
	got := failClosedActionSnapshot(IntentResponseEnvelope{Policy: IntentPolicyEnvelope{MayAct: true}})
	if hasCommitments(got.Commitments) {
		t.Fatalf("commitments = %#v, want none", got.Commitments)
	}
	if got.Policy.MayAct {
		t.Fatal("action still allowed")
	}
}

func TestAskUsesImportedEvidenceAndPersistsExpansions(t *testing.T) {
	isolateSources(t)
	tg, ctx := openTestTideglass(t)
	sessionDir := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"type":"event_msg","payload":{"type":"user_message","message":"please verify the live CI state and use a worktree before changing the OpenClaw release flow"}}` + "\n"
	if err := os.WriteFile(filepath.Join(sessionDir, "rollout.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tg.Ingest(ctx, IngestOptions{Kind: "codex", Path: sessionDir}); err != nil {
		t.Fatal(err)
	}
	query := "verify live CI evidence before release"
	result, err := tg.Ask(ctx, AskOptions{Kind: "work.project.start", Query: query, Explain: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent.Kind != "work.project.start" || result.Query != query {
		t.Fatalf("intent=%#v query=%q", result.Intent, result.Query)
	}
	if want := len(expandQuery("work.project.start", query)); len(result.ExpandedQuestions) != want {
		t.Fatalf("expanded questions = %d, want %d", len(result.ExpandedQuestions), want)
	}
	var codexHits int
	for _, coverage := range result.SourceCoverage {
		if coverage.SourceID == "codex" {
			codexHits = coverage.Hits
		}
	}
	if codexHits == 0 {
		t.Fatalf("codex coverage = %#v, want hits", result.SourceCoverage)
	}
	if len(result.Evidence) == 0 {
		t.Fatal("explain=true should return evidence")
	}
	seen := map[string]bool{}
	for _, ev := range result.Evidence {
		if seen[ev.ID] {
			t.Fatalf("duplicate evidence %q", ev.ID)
		}
		seen[ev.ID] = true
	}
	var queries, expansions int
	if err := tg.db.QueryRowContext(ctx, `select count(*) from queries`).Scan(&queries); err != nil {
		t.Fatal(err)
	}
	if err := tg.db.QueryRowContext(ctx, `select count(*) from query_expansions`).Scan(&expansions); err != nil {
		t.Fatal(err)
	}
	if queries != 1 || expansions != len(result.ExpandedQuestions) {
		t.Fatalf("persisted queries=%d expansions=%d", queries, expansions)
	}
	var linked *ClaimOut
	for i := range result.Claims {
		if len(result.Claims[i].Evidence) > 0 {
			linked = &result.Claims[i]
			break
		}
	}
	if linked == nil {
		t.Fatalf("claims = %#v, want one linked to evidence", result.Claims)
	}
	evidence, err := tg.Evidence(ctx, linked.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence) == 0 || evidence[0].SourceID != "codex" || evidence[0].Snippet == "" {
		t.Fatalf("evidence for %s = %#v", linked.ID, evidence)
	}
	none, err := tg.Evidence(ctx, "clm_missing")
	if err != nil || len(none) != 0 {
		t.Fatalf("missing claim evidence = %#v, %v", none, err)
	}
}

func TestAskWithoutExplainOmitsEvidence(t *testing.T) {
	isolateSources(t)
	tg, ctx := openTestTideglass(t)
	result, err := tg.Ask(ctx, AskOptions{Query: "dinner with friends on friday"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Intent.Kind != "social.dinner" {
		t.Fatalf("kind = %q, want social.dinner", result.Intent.Kind)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("evidence = %#v, want none without explain", result.Evidence)
	}
	if len(result.UnresolvedQuestions) == 0 {
		t.Fatal("empty profile should leave questions unresolved")
	}
}

func TestDoctorSummarisesSourceHealth(t *testing.T) {
	isolateSources(t)
	tg, ctx := openTestTideglass(t)
	report, err := tg.Doctor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.DBPath != tg.Path() || report.Schema != schemaVersion || report.GeneratedAt == "" {
		t.Fatalf("report header = %#v", report)
	}
	ids := map[string]bool{}
	for _, check := range report.Checks {
		ids[check.ID] = true
	}
	if !ids["db.open"] || !ids["schema.version"] {
		t.Fatalf("checks = %#v", report.Checks)
	}
	wantState := "ok"
	for _, source := range report.Sources {
		if source.Health == "error" || source.Health == "partial" {
			wantState = "warning"
			if !ids["source."+source.ID] {
				t.Fatalf("unhealthy source %s has no check", source.ID)
			}
		}
	}
	if report.OverallState != wantState {
		t.Fatalf("overall = %q, want %q", report.OverallState, wantState)
	}
}

func TestNewServiceHandlerReadsTokenFromEnvironment(t *testing.T) {
	isolateSources(t)
	tg, _ := openTestTideglass(t)
	t.Setenv("TIDEGLASS_SERVICE_TOKEN", "  env-token  ")
	server := httptest.NewServer(NewServiceHandler(tg))
	defer server.Close()
	body := `{"uri":"tideglass://disclosure/social.dinner/venue"}`
	denied, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", strings.NewReader(body), "wrong")
	if err != nil {
		t.Fatal(err)
	}
	defer denied.Body.Close()
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", denied.StatusCode)
	}
	allowed, err := authedHTTP(t, http.MethodPost, server.URL+"/resolve", "application/json", strings.NewReader(body), "env-token")
	if err != nil {
		t.Fatal(err)
	}
	defer allowed.Body.Close()
	if allowed.StatusCode != http.StatusOK {
		t.Fatalf("env token status = %d", allowed.StatusCode)
	}
}

func TestRetrieveDispatchesBySourceID(t *testing.T) {
	tg, ctx := openTestTideglass(t)
	if got, err := tg.retrieve(ctx, SourceStatus{ID: "unknown"}, "anything", 5); err != nil || got != nil {
		t.Fatalf("unknown source = %#v, %v", got, err)
	}
	for _, id := range []string{"slacrawl", "discrawl", "notcrawl", "graincrawl"} {
		_, err := tg.retrieve(ctx, SourceStatus{ID: id, Locator: "  "}, "openclaw", 5)
		if err == nil || !strings.Contains(err.Error(), id+" database locator is empty") {
			t.Fatalf("%s empty locator err = %v", id, err)
		}
	}
	for _, id := range []string{"codex", "chatgpt", "claude"} {
		got, err := tg.retrieve(ctx, SourceStatus{ID: id}, "openclaw", 5)
		if err != nil || len(got) != 0 {
			t.Fatalf("%s on empty db = %#v, %v", id, got, err)
		}
	}
}

func TestExpandQueryHasTemplatesForEveryKind(t *testing.T) {
	for _, kind := range []string{"social.dinner", "social.dating", "work.new_job", "agent.delegation", "work.project.start", "something.else"} {
		expansions := expandQuery(kind, "q")
		if len(expansions) == 0 {
			t.Fatalf("%s has no expansions", kind)
		}
		seen := map[string]bool{}
		for _, expansion := range expansions {
			if expansion.Question == "" || expansion.Probe == "" || expansion.TargetClaimKind == "" {
				t.Fatalf("%s incomplete expansion %#v", kind, expansion)
			}
			if seen[expansion.TargetClaimKind] {
				t.Fatalf("%s duplicates target %s", kind, expansion.TargetClaimKind)
			}
			seen[expansion.TargetClaimKind] = true
		}
	}
}

func TestRedactedBlockingQuestions(t *testing.T) {
	if got := redactedBlockingQuestions("social.dinner", nil, nil, nil, ""); got != nil {
		t.Fatalf("no redactions = %#v", got)
	}
	got := redactedBlockingQuestions("social.dinner", []string{"boundary.social.topic", "preference.food.cuisine", "custom.required", "policy.action.constraints"}, []string{" custom.required "}, nil, "plan")
	if !hasQuestionSlot(got, "boundary.social.topic") || !hasQuestionSlot(got, "custom.required") {
		t.Fatalf("boundary and required slots must block: %#v", got)
	}
	if hasQuestionSlot(got, "policy.action.constraints") {
		t.Fatal("action constraints only block in act_gate mode")
	}
	for _, question := range got {
		if !question.BlocksAction || question.Priority != "critical" {
			t.Fatalf("question not critical/blocking: %#v", question)
		}
	}
	gate := redactedBlockingQuestions("social.dinner", []string{"policy.action.constraints"}, nil, nil, "act_gate")
	if !hasQuestionSlot(gate, "policy.action.constraints") {
		t.Fatalf("act_gate = %#v", gate)
	}
	existing := []IntentQuestion{questionForSlot("boundary.social.topic", "critical", true)}
	again := redactedBlockingQuestions("social.dinner", []string{"boundary.social.topic", "boundary.social.topic"}, nil, existing, "")
	if len(again) != 0 {
		t.Fatalf("existing blocking question duplicated: %#v", again)
	}
}

func TestDiscoverCrawlBarReadsAppsAndPrimaryDatabase(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "crawlbar")
	contents := `#!/bin/sh
if [ "$1" = "apps" ]; then
  echo '[{"id":"slacrawl","display_name":"Slack","available":true,"binary_path":"/bin/slacrawl","enabled":true},{"id":"gone","display_name":"Gone","available":false}]'
  exit 0
fi
if [ "$1" = "status" ] && [ "$3" = "slacrawl" ]; then
  echo '[{"database_path":"","databases":[{"path":"/data/other.db","is_primary":false},{"path":"/data/slack.db","is_primary":true}]}]'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(script, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIDEGLASS_CRAWLBAR", script)
	sources, err := discoverCrawlBar(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %#v, want only available apps", sources)
	}
	got := sources[0]
	if got.ID != "slacrawl" || got.Label != "Slack" || got.Kind != "crawl" || got.Locator != "/data/slack.db" {
		t.Fatalf("source = %#v", got)
	}
	if got.Metadata["binary_path"] != "/bin/slacrawl" || got.Metadata["enabled"] != "true" {
		t.Fatalf("metadata = %#v", got.Metadata)
	}
	if path := crawlbarDatabasePath(context.Background(), script, "unknown"); path != "" {
		t.Fatalf("failing status should give empty path, got %q", path)
	}
}

func TestDiscoverCrawlBarMissingBinary(t *testing.T) {
	t.Setenv("TIDEGLASS_CRAWLBAR", filepath.Join(t.TempDir(), "missing"))
	if _, err := discoverCrawlBar(context.Background()); err == nil {
		t.Fatal("expected error for missing crawlbar")
	}
}

func TestSmallHelpers(t *testing.T) {
	if !sqliteDuplicateColumnError(errors.New("SQL logic error: Duplicate column name: x")) {
		t.Fatal("duplicate column not detected")
	}
	if sqliteDuplicateColumnError(nil) || sqliteDuplicateColumnError(errors.New("no such table")) {
		t.Fatal("false positive")
	}
	if got := compactEvidenceIDs([]string{"", "ev1", "", "ev2"}); len(got) != 2 || got[0] != "ev1" || got[1] != "ev2" {
		t.Fatalf("compact = %#v", got)
	}
	if got := compactEvidenceIDs([]string{""}); got != nil {
		t.Fatalf("compact empty = %#v", got)
	}
	unique := uniqueEvidence([]retrievedEvidence{{EvidenceID: "a"}, {EvidenceID: "b"}, {EvidenceID: "a", Snippet: "dup"}})
	if len(unique) != 2 || unique[0].EvidenceID != "a" || unique[0].Snippet != "" || unique[1].EvidenceID != "b" {
		t.Fatalf("unique = %#v", unique)
	}
}

func TestRenderAgentContext(t *testing.T) {
	intent := IntentOut{Title: "Dinner", Kind: "social.dinner"}
	claims := []ClaimOut{{Kind: "preference.food.cuisine", Value: "ramen\x1b[31m\nplease", Confidence: 0.8}}
	text := renderAgentContext(intent, claims, []string{"Any allergies?"}, 0)
	for _, want := range []string{"intent: Dinner (social.dinner)", "preferences and constraints:", "- preference.food.cuisine: ", "(confidence 0.80)", "unresolved:", "- Any allergies?"} {
		if !strings.Contains(text, want) {
			t.Fatalf("context missing %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b") {
		t.Fatalf("control sequence leaked: %q", text)
	}
	if got := renderAgentContext(intent, nil, nil, 0); got != "intent: Dinner (social.dinner)" {
		t.Fatalf("bare context = %q", got)
	}
	truncated := renderAgentContext(intent, claims, nil, 20)
	if !strings.HasSuffix(truncated, "\n...") || len(truncated) > 24 {
		t.Fatalf("truncated = %q", truncated)
	}
}

func TestPathAndDefaultDBPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err := defaultDBPath("")
	if err != nil || got != filepath.Join(home, ".tideglass", "tideglass.db") {
		t.Fatalf("default = %q, %v", got, err)
	}
	got, err = defaultDBPath("~/custom.db")
	if err != nil || got != filepath.Join(home, "custom.db") {
		t.Fatalf("tilde = %q, %v", got, err)
	}
	tg, _ := openTestTideglass(t)
	if !strings.HasSuffix(tg.Path(), "tideglass.db") {
		t.Fatalf("path = %q", tg.Path())
	}
}

func TestPrintText(t *testing.T) {
	out := captureStdout(t, func() {
		if err := PrintText("%s=%d\n", "claims", 3); err != nil {
			t.Fatal(err)
		}
	})
	if out != "claims=3\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestImportAssistantDirWalksJSONFilesWithLimit(t *testing.T) {
	root := t.TempDir()
	long := "I always want agents to verify the live CI state before merging anything"
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.json", `{"memory":"`+long+` (first)"}`)
	write("nested/b.JSON", `{"memory":"`+long+` (second)"}`)
	write("notes.txt", `{"memory":"`+long+` (ignored)"}`)
	write("nested/short.json", `{"memory":"too short"}`)

	all, err := importAssistantDir("chatgpt", root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("artifacts = %d, want 2 (json only, short text skipped)", len(all))
	}
	for _, artifact := range all {
		if strings.Contains(artifact.Snippet, "ignored") {
			t.Fatalf("non-json file imported: %q", artifact.Snippet)
		}
	}
	limited, err := importAssistantDir("chatgpt", root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limited artifacts = %d, want 1", len(limited))
	}
	write("broken.json", `{not json`)
	if _, err := importAssistantDir("chatgpt", root, 0); err == nil || !strings.Contains(err.Error(), "broken.json") {
		t.Fatalf("broken json err = %v", err)
	}
}
