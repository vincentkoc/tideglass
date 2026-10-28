package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/openclaw/crawlkit/store"
	"github.com/openclaw/crawlkit/vector"
)

var wordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*`)

func (t *Tideglass) Ask(ctx context.Context, opts AskOptions) (AskResult, error) {
	kind := normalizeKind(opts.Kind, opts.Query)
	intent, err := t.ensureIntent(ctx, kind, titleForKind(kind))
	if err != nil {
		return AskResult{}, err
	}
	queryID := id("qry")
	if _, err := t.db.ExecContext(ctx, `insert into queries(id,intent_id,raw_query,normalized_query,kind_hint,expansion_version,created_at) values(?,?,?,?,?,?,?)`,
		queryID, intent.ID, opts.Query, normalizeText(opts.Query), kind, "templates.v1", now()); err != nil {
		return AskResult{}, err
	}
	expansions := expandQuery(kind, opts.Query)
	for _, expansion := range expansions {
		if _, err := t.db.ExecContext(ctx, `insert into query_expansions(id,query_id,question,probe,target_claim_kind,priority,source_policy,created_at) values(?,?,?,?,?,?,?,?)`,
			id("qex"), queryID, expansion.Question, expansion.Probe, expansion.TargetClaimKind, 0, "auto", now()); err != nil {
			return AskResult{}, err
		}
	}
	var allEvidence []retrievedEvidence
	var coverage []SourceCoverage
	sourceList, err := t.Sources(ctx, SourceOptions{Probe: true})
	if err != nil {
		return AskResult{}, err
	}
	queries := append([]ExpansionOut{{Question: "original query", Probe: opts.Query, TargetClaimKind: "intent"}}, expansions...)
	for _, source := range sourceList.Sources {
		if !sourceAllowed(kind, source.ID) {
			continue
		}
		hits := 0
		var sourceErr string
		for _, expansion := range queries {
			found, err := t.retrieve(ctx, source, expansion.Probe, 5)
			if err != nil {
				sourceErr = err.Error()
				continue
			}
			hits += len(found)
			allEvidence = append(allEvidence, found...)
		}
		coverage = append(coverage, SourceCoverage{SourceID: source.ID, Health: source.Health, Hits: hits, Error: sourceErr})
	}
	claims := extractClaims(kind, opts.Query, allEvidence)
	var outClaims []ClaimOut
	for _, claim := range claims {
		claimID, err := t.insertClaim(ctx, intent.ID, claim)
		if err != nil {
			return AskResult{}, err
		}
		if claim.EvidenceID != "" {
			if err := t.linkClaimEvidence(ctx, claimID, claim.EvidenceID, "supporting"); err != nil {
				return AskResult{}, err
			}
		}
		if err := t.embedText(ctx, "claim", claimID, claim.Value); err != nil {
			return AskResult{}, err
		}
		outClaims = append(outClaims, ClaimOut{
			ID:         claimID,
			Kind:       claim.Kind,
			Value:      claim.Value,
			Confidence: claim.Confidence,
			Status:     "active",
			SourceMode: claim.SourceMode,
			Evidence:   compactEvidenceIDs([]string{claim.EvidenceID}),
		})
	}
	if err := t.embedText(ctx, "query", queryID, opts.Query); err != nil {
		return AskResult{}, err
	}
	evidenceOut := make([]EvidenceOut, 0, len(allEvidence))
	if opts.Explain {
		for _, ev := range uniqueEvidence(allEvidence) {
			evidenceOut = append(evidenceOut, EvidenceOut{ID: ev.EvidenceID, SourceID: ev.SourceID, Locator: ev.Locator, Snippet: ev.Snippet})
		}
	}
	return AskResult{
		Intent:              intent,
		Query:               opts.Query,
		ExpandedQuestions:   expansions,
		Claims:              outClaims,
		Evidence:            evidenceOut,
		UnresolvedQuestions: unresolvedQuestions(kind, outClaims),
		SourceCoverage:      coverage,
	}, nil
}

func (t *Tideglass) embedText(ctx context.Context, ownerKind, ownerID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	values := lexicalVector(text, 64)
	blob, err := vector.EncodeFloat32(values)
	if err != nil {
		return err
	}
	_, err = t.db.ExecContext(ctx, `
insert into embeddings(id,owner_kind,owner_id,model,dimensions,vector,content_hash,created_at)
values(?,?,?,?,?,?,?,?)
on conflict(owner_kind, owner_id, model) do update set vector=excluded.vector,content_hash=excluded.content_hash,created_at=excluded.created_at`,
		id("emb"), ownerKind, ownerID, "tideglass-lexical-v1", len(values), blob, hashText(text), now())
	return err
}

func (t *Tideglass) retrieve(ctx context.Context, source SourceStatus, query string, limit int) ([]retrievedEvidence, error) {
	switch source.ID {
	case "slacrawl":
		return t.retrieveFTS(ctx, source, source.Locator, "message_fts", "message_key", "content", query, limit)
	case "discrawl":
		return t.retrieveFTS(ctx, source, source.Locator, "message_fts", "message_id", "content", query, limit)
	case "notcrawl":
		return t.retrieveFTS(ctx, source, source.Locator, "page_fts", "page_id", "body", query, limit)
	case "graincrawl":
		return t.retrieveFTS(ctx, source, source.Locator, "notes_fts", "note_id", "summary_text", query, limit)
	case "gitcrawl":
		return t.retrieveGitcrawl(ctx, source, query, limit)
	case "codex", "chatgpt", "claude":
		return t.retrieveImported(ctx, source, query, limit)
	default:
		return nil, nil
	}
}

func (t *Tideglass) retrieveFTS(ctx context.Context, source SourceStatus, path, table, idColumn, contentColumn, query string, limit int) ([]retrievedEvidence, error) {
	path = expandHome(path)
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s database locator is empty", source.ID)
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer ro.Close()
	ftsQuery := ftsQuery(query)
	if ftsQuery == "" {
		return nil, nil
	}
	sqlText := fmt.Sprintf(`select %s, snippet(%s, -1, '[', ']', '...', 24) from %s where %s match ? limit ?`,
		store.QuoteIdent(idColumn), store.QuoteIdent(table), store.QuoteIdent(table), store.QuoteIdent(table))
	rows, err := ro.DB().QueryContext(ctx, sqlText, ftsQuery, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retrievedEvidence
	for rows.Next() {
		var externalID, snippet string
		if err := rows.Scan(&externalID, &snippet); err != nil {
			return nil, err
		}
		locator, _ := json.Marshal(map[string]string{"database_path": path, "table": table, "id": externalID})
		evID, err := t.upsertArtifactEvidence(ctx, artifact{
			SourceID:     source.ID,
			ExternalID:   table + ":" + externalID,
			Kind:         "fts_row",
			Title:        table + " " + externalID,
			ContentHash:  hashText(snippet),
			MetadataJSON: "{}",
			Snippet:      snippet,
			LocatorJSON:  string(locator),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, retrievedEvidence{EvidenceID: evID, SourceID: source.ID, Locator: string(locator), Snippet: snippet})
	}
	return out, rows.Err()
}

func (t *Tideglass) retrieveGitcrawl(ctx context.Context, source SourceStatus, query string, limit int) ([]retrievedEvidence, error) {
	path := expandHome(source.Locator)
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%s database locator is empty", source.ID)
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		return nil, err
	}
	defer ro.Close()
	terms := words(query)
	if len(terms) == 0 {
		return nil, nil
	}
	bodyExpr := "''"
	if sqliteColumnExists(ctx, ro.DB(), "threads", "body_excerpt") {
		bodyExpr = "coalesce(body_excerpt,'')"
	} else if sqliteColumnExists(ctx, ro.DB(), "threads", "body") {
		bodyExpr = "coalesce(body,'')"
	}
	var where []string
	var args []any
	for _, term := range terms[:minInt(len(terms), 5)] {
		where = append(where, `(lower(title) like ? or lower(`+bodyExpr+`) like ?)`)
		pattern := "%" + strings.ToLower(term) + "%"
		args = append(args, pattern, pattern)
	}
	args = append(args, limit)
	rows, err := ro.DB().QueryContext(ctx, `select id, title, `+bodyExpr+`, html_url from threads where `+strings.Join(where, " or ")+` order by updated_at_gh desc limit ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retrievedEvidence
	for rows.Next() {
		var rowID int64
		var title, body, url string
		if err := rows.Scan(&rowID, &title, &body, &url); err != nil {
			return nil, err
		}
		snippet := trimForSnippet(title+". "+body, 1000)
		locator, _ := json.Marshal(map[string]any{"database_path": path, "table": "threads", "id": rowID, "url": url})
		evID, err := t.upsertArtifactEvidence(ctx, artifact{
			SourceID:     source.ID,
			ExternalID:   fmt.Sprintf("threads:%d", rowID),
			Kind:         "github_thread",
			Title:        title,
			ContentHash:  hashText(snippet),
			MetadataJSON: "{}",
			Snippet:      snippet,
			LocatorJSON:  string(locator),
		})
		if err != nil {
			return nil, err
		}
		out = append(out, retrievedEvidence{EvidenceID: evID, SourceID: source.ID, Locator: string(locator), Snippet: snippet})
	}
	return out, rows.Err()
}

func (t *Tideglass) retrieveImported(ctx context.Context, source SourceStatus, query string, limit int) ([]retrievedEvidence, error) {
	queryText := ftsQuery(query)
	if queryText == "" {
		return nil, nil
	}
	rows, err := t.db.QueryContext(ctx, `
select e.id, e.locator_json, f.snippet
from evidence_fts f
join evidence e on e.id = f.evidence_id
where f.snippet match ? and f.source_id = ?
limit ?`, queryText, source.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []retrievedEvidence
	for rows.Next() {
		var ev retrievedEvidence
		ev.SourceID = source.ID
		if err := rows.Scan(&ev.EvidenceID, &ev.Locator, &ev.Snippet); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func extractClaims(kind, query string, evidence []retrievedEvidence) []candidateClaim {
	var claims []candidateClaim
	seen := map[string]bool{}
	add := func(kind, value string, confidence float64, sourceMode string, evidenceID string) {
		key := kind + "\x00" + normalizeText(value)
		if seen[key] || strings.TrimSpace(value) == "" {
			return
		}
		seen[key] = true
		claims = append(claims, candidateClaim{Kind: kind, Value: value, Confidence: confidence, SourceMode: sourceMode, EvidenceID: evidenceID})
	}
	all := strings.ToLower(query + "\n" + strings.Join(snippets(evidence), "\n"))
	tokens := rawTokenSet(all)
	hasToken := func(targets ...string) bool {
		for _, target := range targets {
			if tokens[strings.ToLower(target)] {
				return true
			}
		}
		return false
	}
	switch kind {
	case "work.project.start", "work.release", "agent.delegation":
		if hasToken("live", "evidence", "proof") || (hasToken("exact") && hasToken("sha")) {
			add("preference.agent.evidence", "Prefer live, concrete evidence over stale assumptions; include exact proof such as command output, CI state, or landed SHA when relevant.", 0.88, "inferred", findEvidence(evidence, "live", "evidence", "proof", "exact", "sha"))
		}
		if hasToken("test", "tests", "validate", "validation", "check", "checks", "ci") {
			add("preference.project.validation", "Prefer scoped verification first, then broader gates when risk or user request requires it.", 0.82, "inferred", findEvidence(evidence, "test", "validate", "check", "ci", "proof"))
		}
		if hasToken("worktree", "worktrees", "gwt", "branch", "branches") {
			add("preference.project.branching", "Prefer fresh, scoped worktrees and explicit branch/worktree state for implementation lanes.", 0.8, "inferred", findEvidence(evidence, "worktree", "gwt", "branch"))
		}
		if hasToken("concise", "terse", "fluff") {
			add("preference.agent.communication", "Use terse, operational updates with concrete state and next action; avoid filler.", 0.86, "inferred", findEvidence(evidence, "concise", "terse", "fluff", "status"))
		}
		killSignal := hasToken("kill", "kills", "killing", "terminate", "terminates", "terminated", "interrupt", "interrupts", "stop", "stops")
		processTarget := hasToken("process", "processes", "tmux", "mosh", "ssh", "terminal", "terminals", "codex", "agent")
		if killSignal && processTarget {
			add("boundary.agent.process_kill", "Do not kill broad Codex, agent, tmux, SSH, mosh, or terminal processes without explicit current-turn scope.", 0.9, "inferred", findEvidence(evidence, "kill", "process", "tmux", "mosh"))
		}
		if hasToken("push", "pushes", "merge", "merges", "external", "land", "lands", "landing", "pr", "prs") {
			add("boundary.agent.external_action", "Treat pushes, merges, PR updates, and other external actions as high-integrity operations requiring current state and clear permission.", 0.78, "inferred", findEvidence(evidence, "push", "merge", "external", "land", "permission"))
		}
	case "social.dinner":
		for _, ev := range evidence {
			s := strings.ToLower(ev.Snippet)
			if strings.Contains(s, "sushi") || strings.Contains(s, "ramen") || strings.Contains(s, "japanese") {
				add("preference.food.cuisine", "Possible preference for Japanese food such as sushi or ramen.", 0.62, "inferred", ev.EvidenceID)
			}
			if strings.Contains(s, "spicy") || strings.Contains(s, "chili") {
				add("preference.food.cuisine", "Possible preference for spicy food.", 0.58, "inferred", ev.EvidenceID)
			}
			if strings.Contains(s, "allerg") {
				add("preference.food.allergy", "Potential allergy-related dinner constraint found in source evidence; needs direct confirmation.", 0.55, "inferred", ev.EvidenceID)
			}
			if strings.Contains(s, "budget") || strings.Contains(s, "expensive") || strings.Contains(s, "cheap") {
				add("preference.food.budget", "Budget comfort may matter for dinner planning; source evidence is weak and should be confirmed.", 0.5, "inferred", ev.EvidenceID)
			}
		}
	case "social.dating":
		if strings.Contains(all, "safety") || strings.Contains(all, "boundary") {
			add("boundary.dating.safety", "Dating profile should preserve explicit safety and boundary preferences instead of inferring sensitive traits.", 0.75, "inferred", findEvidence(evidence, "safety", "boundary"))
		}
		if strings.Contains(all, "communication") || strings.Contains(all, "text") {
			add("preference.dating.communication_style", "Communication style is a high-value dating preference to capture explicitly.", 0.6, "inferred", findEvidence(evidence, "communication", "text"))
		}
	case "work.new_job":
		if strings.Contains(all, "docs") || strings.Contains(all, "documentation") {
			add("preference.work.documentation", "Prefer clear written operating context and reusable documentation for onboarding.", 0.72, "inferred", findEvidence(evidence, "docs", "documentation", "onboarding"))
		}
		if strings.Contains(all, "meeting") || strings.Contains(all, "async") {
			add("preference.work.meeting_style", "Meeting and async communication preferences should be captured explicitly for new-job onboarding.", 0.62, "inferred", findEvidence(evidence, "meeting", "async"))
		}
	}
	if len(claims) == 0 && len(evidence) > 0 {
		add("context.intent.evidence_found", "Relevant evidence exists, but the current extractor could not promote it to a typed preference without more confidence.", 0.45, "inferred", evidence[0].EvidenceID)
	}
	return claims
}

func expandQuery(kind, query string) []ExpansionOut {
	switch kind {
	case "social.dinner":
		return []ExpansionOut{
			{"dietary restrictions and allergies", "vegetarian vegan halal kosher gluten dairy allergy allergic cannot eat", "preference.food.dietary_restriction"},
			{"preferred cuisine", "restaurants cuisine dinner favorite spicy sushi ramen japanese thai italian", "preference.food.cuisine"},
			{"budget comfort", "budget expensive cheap split bill restaurant price", "preference.food.budget"},
			{"group and social energy", "quiet loud strangers networking energy introvert extrovert group size", "preference.social.energy"},
			{"topics to avoid", "avoid uncomfortable topic dinner strangers", "boundary.social.topic"},
		}
	case "social.dating":
		return []ExpansionOut{
			{"relationship goal", "dating relationship looking for partner casual serious", "preference.dating.relationship_goal"},
			{"communication style", "dating communication texting call direct slow reply", "preference.dating.communication_style"},
			{"activities", "date activity dinner coffee walk museum bar", "preference.dating.activity"},
			{"boundaries and safety", "dating safety boundary dealbreaker avoid uncomfortable", "boundary.dating.safety"},
		}
	case "work.new_job":
		return []ExpansionOut{
			{"communication defaults", "communication async slack meeting concise direct", "preference.work.communication"},
			{"feedback style", "feedback review direct critique", "preference.work.feedback"},
			{"documentation preference", "docs documentation onboarding runbook spec", "preference.work.documentation"},
			{"focus boundaries", "focus time interruption meeting calendar", "boundary.work.focus_time"},
		}
	case "agent.delegation":
		return []ExpansionOut{
			{"agent autonomy", "do the work end to end don't stop at commentary autonomous", "preference.agent.autonomy"},
			{"evidence standard", "live evidence proof exact state verify", "preference.agent.evidence"},
			{"communication style", "terse concise no fluff status update", "preference.agent.communication"},
			{"external action boundary", "push merge PR land confirm permission", "boundary.agent.external_action"},
			{"process safety", "kill process tmux mosh ssh codex session", "boundary.agent.process_kill"},
		}
	default:
		return []ExpansionOut{
			{"validation preference", "test validate proof CI live evidence exact SHA", "preference.project.validation"},
			{"scope control", "scope no unrelated refactor surgical targeted", "preference.project.scope_control"},
			{"branching", "worktree branch gwt rebase main", "preference.project.branching"},
			{"communication", "terse concise status update no fluff", "preference.agent.communication"},
			{"external action boundary", "push merge PR land confirm permission", "boundary.agent.external_action"},
		}
	}
}

func words(text string) []string {
	raw := wordTokens(text)
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, word := range raw {
		if seen[word] {
			continue
		}
		seen[word] = true
		out = append(out, word)
	}
	return out
}

func wordTokens(text string) []string {
	raw := wordRE.FindAllString(strings.ToLower(text), -1)
	out := make([]string, 0, len(raw))
	for _, word := range raw {
		word = strings.Trim(word, "_")
		if (len(word) < 3 && !shortDomainToken(word)) || stopword(word) {
			continue
		}
		out = append(out, word)
	}
	return out
}

func shortDomainToken(word string) bool {
	switch word {
	case "ci", "pr", "db", "ui", "ai":
		return true
	default:
		return false
	}
}

func stopword(word string) bool {
	switch word {
	case "the", "and", "for", "with", "that", "this", "what", "when", "where", "from", "into", "want", "wants", "should", "would", "could", "about", "there", "their", "they", "them", "you", "your", "user", "person", "looking":
		return true
	default:
		return false
	}
}

func ftsQuery(text string) string {
	terms := words(text)
	if len(terms) > 8 {
		terms = terms[:8]
	}
	var parts []string
	for _, term := range terms {
		parts = append(parts, term+"*")
	}
	return strings.Join(parts, " OR ")
}

func trimForSnippet(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= limit {
		return text
	}
	return strings.TrimSpace(text[:limit])
}

func snippets(evidence []retrievedEvidence) []string {
	out := make([]string, 0, len(evidence))
	for _, ev := range evidence {
		out = append(out, ev.Snippet)
	}
	return out
}

func findEvidence(evidence []retrievedEvidence, keywords ...string) string {
	for _, ev := range evidence {
		snippet := strings.ToLower(ev.Snippet)
		for _, keyword := range keywords {
			if strings.Contains(snippet, strings.ToLower(keyword)) {
				return ev.EvidenceID
			}
		}
	}
	if len(evidence) == 0 {
		return ""
	}
	return evidence[0].EvidenceID
}

func compactEvidenceIDs(ids []string) []string {
	var out []string
	for _, item := range ids {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func uniqueEvidence(items []retrievedEvidence) []retrievedEvidence {
	seen := map[string]bool{}
	var out []retrievedEvidence
	for _, item := range items {
		if seen[item.EvidenceID] {
			continue
		}
		seen[item.EvidenceID] = true
		out = append(out, item)
	}
	return out
}

func lexicalVector(text string, dimensions int) []float32 {
	values := make([]float32, dimensions)
	for _, word := range words(text) {
		sum := sha256.Sum256([]byte(word))
		index := int(sum[0]) % dimensions
		values[index] += 1
	}
	var norm float64
	for _, value := range values {
		norm += float64(value * value)
	}
	if norm == 0 {
		values[0] = 1
		return values
	}
	scale := float32(1 / math.Sqrt(norm))
	for i := range values {
		values[i] *= scale
	}
	return values
}
