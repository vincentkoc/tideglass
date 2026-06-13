package app

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/openclaw/crawlkit/store"
	"github.com/openclaw/crawlkit/vector"
)

const schemaVersion = 1

type Tideglass struct {
	store *store.Store
	db    *sql.DB
	path  string
}

type SourceOptions struct {
	Probe bool
}

type IngestOptions struct {
	Kind  string
	Path  string
	Limit int
}

type AskOptions struct {
	Kind    string
	Query   string
	Explain bool
}

type ProfileOptions struct {
	IntentID string
	Kind     string
	ForAgent string
	Budget   int
}

type EditOptions struct {
	ClaimID string
	Value   string
	Reason  string
}

type ExportOptions struct {
	IntentID string
	Kind     string
	Format   string
	Out      string
}

type SourceStatus struct {
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Label        string            `json:"label"`
	Locator      string            `json:"locator"`
	Health       string            `json:"health"`
	Capabilities []string          `json:"capabilities"`
	Counts       map[string]int64  `json:"counts,omitempty"`
	LastProbeAt  string            `json:"last_probe_at,omitempty"`
	LastError    string            `json:"last_error,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type SourceList struct {
	Sources []SourceStatus `json:"sources"`
}

type IngestResult struct {
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	Artifacts int    `json:"artifacts"`
	Evidence  int    `json:"evidence"`
	Claims    int    `json:"claims"`
}

type AskResult struct {
	Intent              IntentOut        `json:"intent"`
	Query               string           `json:"query"`
	ExpandedQuestions   []ExpansionOut   `json:"expanded_questions"`
	Claims              []ClaimOut       `json:"claims"`
	Evidence            []EvidenceOut    `json:"evidence,omitempty"`
	UnresolvedQuestions []string         `json:"unresolved_questions"`
	SourceCoverage      []SourceCoverage `json:"source_coverage"`
}

type IntentOut struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type ExpansionOut struct {
	Question        string `json:"question"`
	Probe           string `json:"probe"`
	TargetClaimKind string `json:"target_claim_kind"`
}

type ClaimOut struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Value      string   `json:"value"`
	Confidence float64  `json:"confidence"`
	Status     string   `json:"status"`
	SourceMode string   `json:"source_mode"`
	Evidence   []string `json:"evidence,omitempty"`
}

type EvidenceOut struct {
	ID       string `json:"id"`
	SourceID string `json:"source_id"`
	Locator  string `json:"locator"`
	Snippet  string `json:"snippet"`
}

type SourceCoverage struct {
	SourceID string `json:"source_id"`
	Health   string `json:"health"`
	Hits     int    `json:"hits"`
	Error    string `json:"error,omitempty"`
}

type ProfileResult struct {
	Intent              IntentOut        `json:"intent"`
	ForAgent            string           `json:"for_agent,omitempty"`
	Claims              []ClaimOut       `json:"claims"`
	UnresolvedQuestions []string         `json:"unresolved_questions,omitempty"`
	SourceCoverage      []SourceCoverage `json:"source_coverage,omitempty"`
	Text                string           `json:"text,omitempty"`
}

type EditResult struct {
	EditID  string `json:"edit_id"`
	ClaimID string `json:"claim_id"`
	Value   string `json:"value"`
}

type ExportResult struct {
	Path    string `json:"path"`
	Format  string `json:"format"`
	Records int    `json:"records"`
}

type DoctorResult struct {
	DBPath       string         `json:"db_path"`
	Schema       int            `json:"schema"`
	Sources      []SourceStatus `json:"sources"`
	Checks       []CheckResult  `json:"checks"`
	GeneratedAt  string         `json:"generated_at"`
	OverallState string         `json:"overall_state"`
}

type CheckResult struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type artifact struct {
	SourceID     string
	ExternalID   string
	Kind         string
	Title        string
	AuthorRef    string
	CreatedAt    string
	UpdatedAt    string
	ContentHash  string
	MetadataJSON string
	Snippet      string
	LocatorJSON  string
}

type candidateClaim struct {
	Kind       string
	Value      string
	Confidence float64
	SourceMode string
	EvidenceID string
}

type retrievedEvidence struct {
	EvidenceID string
	SourceID   string
	Locator    string
	Snippet    string
}

const assistantJSONLimit = 512 * 1024 * 1024

var wordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]{2,}`)

func Open(ctx context.Context, dbPath string) (*Tideglass, error) {
	path, err := defaultDBPath(dbPath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create tideglass dir: %w", err)
	}
	st, err := store.Open(ctx, store.Options{Path: path, Schema: schemaSQL, SchemaVersion: schemaVersion})
	if err != nil {
		return nil, err
	}
	tg := &Tideglass{store: st, db: st.DB(), path: path}
	if err := tg.ensureEvidenceSearch(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	return tg, nil
}

func (t *Tideglass) Close() error {
	return t.store.Close()
}

func (t *Tideglass) Path() string {
	return t.path
}

func (t *Tideglass) Sources(ctx context.Context, opts SourceOptions) (SourceList, error) {
	sources := discoverStaticSources()
	crawlbarSources, _ := discoverCrawlBar(ctx)
	sourceByID := map[string]SourceStatus{}
	for _, src := range sources {
		sourceByID[src.ID] = src
	}
	for _, src := range crawlbarSources {
		if existing, ok := sourceByID[src.ID]; ok {
			existing.Label = src.Label
			if strings.TrimSpace(src.Locator) != "" {
				existing.Locator = src.Locator
			}
			existing.Metadata = mergeStringMaps(existing.Metadata, src.Metadata)
			sourceByID[src.ID] = existing
			continue
		}
		sourceByID[src.ID] = src
	}
	out := make([]SourceStatus, 0, len(sourceByID))
	for _, source := range sourceByID {
		if opts.Probe {
			source = probeSource(ctx, source)
		}
		if err := t.upsertSource(ctx, source); err != nil {
			return SourceList{}, err
		}
		out = append(out, source)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return SourceList{Sources: out}, nil
}

func (t *Tideglass) Ingest(ctx context.Context, opts IngestOptions) (IngestResult, error) {
	kind := strings.TrimSpace(strings.ToLower(opts.Kind))
	if kind == "" {
		return IngestResult{}, errors.New("ingest kind is required")
	}
	var artifacts []artifact
	var claims []candidateClaim
	var err error
	switch kind {
	case "codex":
		artifacts, claims, err = importCodex(opts.Path, opts.Limit)
	case "chatgpt":
		artifacts, claims, err = importAssistantExport("chatgpt", opts.Path, opts.Limit)
	case "claude":
		artifacts, claims, err = importAssistantExport("claude", opts.Path, opts.Limit)
	default:
		return IngestResult{}, fmt.Errorf("unsupported ingest kind %q", opts.Kind)
	}
	if err != nil {
		return IngestResult{}, err
	}
	source := SourceStatus{
		ID:           kind,
		Kind:         "import",
		Label:        kind,
		Locator:      opts.Path,
		Health:       "ok",
		Capabilities: []string{"metadata", "text"},
		LastProbeAt:  now(),
	}
	if err := t.upsertSource(ctx, source); err != nil {
		return IngestResult{}, err
	}
	var evidenceCount int
	importedClaims := 0
	for _, art := range artifacts {
		if art.SourceID == "" {
			art.SourceID = kind
		}
		evidenceID, err := t.upsertArtifactEvidence(ctx, art)
		if err != nil {
			return IngestResult{}, err
		}
		evidenceCount++
		if err := t.embedText(ctx, "evidence", evidenceID, art.Snippet); err != nil {
			return IngestResult{}, err
		}
		if (kind == "chatgpt" || kind == "claude") && looksLikeMemory(art.Title, art.Snippet) {
			intent, err := t.ensureIntent(ctx, "agent.delegation", "Agent delegation")
			if err != nil {
				return IngestResult{}, err
			}
			claimID, err := t.insertClaim(ctx, intent.ID, candidateClaim{
				Kind:       "preference.agent.imported_memory",
				Value:      art.Snippet,
				Confidence: 0.9,
				SourceMode: "imported_memory",
				EvidenceID: evidenceID,
			})
			if err != nil {
				return IngestResult{}, err
			}
			if _, err := t.db.ExecContext(ctx, `insert or ignore into claim_evidence(claim_id,evidence_id,role) values(?,?,?)`, claimID, evidenceID, "supporting"); err != nil {
				return IngestResult{}, err
			}
			importedClaims++
		}
	}
	for _, claim := range claims {
		intent, err := t.ensureIntent(ctx, "agent.delegation", "Agent delegation")
		if err != nil {
			return IngestResult{}, err
		}
		claimID, err := t.insertClaim(ctx, intent.ID, claim)
		if err != nil {
			return IngestResult{}, err
		}
		if claim.EvidenceID != "" {
			if _, err := t.db.ExecContext(ctx, `insert or ignore into claim_evidence(claim_id,evidence_id,role) values(?,?,?)`, claimID, claim.EvidenceID, "supporting"); err != nil {
				return IngestResult{}, err
			}
		}
		importedClaims++
	}
	return IngestResult{Kind: kind, Path: opts.Path, Artifacts: len(artifacts), Evidence: evidenceCount, Claims: importedClaims}, nil
}

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
			_, _ = t.db.ExecContext(ctx, `insert or ignore into claim_evidence(claim_id,evidence_id,role) values(?,?,?)`, claimID, claim.EvidenceID, "supporting")
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

func (t *Tideglass) Profile(ctx context.Context, opts ProfileOptions) (ProfileResult, error) {
	intent, err := t.findIntent(ctx, opts.IntentID, opts.Kind)
	if err != nil {
		return ProfileResult{}, err
	}
	claims, err := t.loadClaims(ctx, intent.ID)
	if err != nil {
		return ProfileResult{}, err
	}
	unresolved := unresolvedQuestions(intent.Kind, claims)
	result := ProfileResult{Intent: intent, ForAgent: opts.ForAgent, Claims: claims, UnresolvedQuestions: unresolved}
	if opts.ForAgent != "" || opts.Budget > 0 {
		result.Text = renderAgentContext(intent, claims, unresolved, opts.Budget)
	}
	return result, nil
}

func (t *Tideglass) EditClaim(ctx context.Context, opts EditOptions) (EditResult, error) {
	if strings.TrimSpace(opts.ClaimID) == "" {
		return EditResult{}, errors.New("claim id is required")
	}
	value := strings.TrimSpace(opts.Value)
	if value == "" {
		return EditResult{}, errors.New("--set value is required")
	}
	patch, _ := json.Marshal(map[string]string{"value": value})
	editID := id("edt")
	if _, err := t.db.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
		editID, opts.ClaimID, "supersede", string(patch), opts.Reason, now()); err != nil {
		return EditResult{}, err
	}
	return EditResult{EditID: editID, ClaimID: opts.ClaimID, Value: value}, nil
}

func (t *Tideglass) Evidence(ctx context.Context, claimID string) ([]EvidenceOut, error) {
	rows, err := t.db.QueryContext(ctx, `
select e.id, sa.source_id, e.locator_json, e.snippet
from claim_evidence ce
join evidence e on e.id = ce.evidence_id
join source_artifacts sa on sa.id = e.source_artifact_id
where ce.claim_id = ?
order by e.observed_at desc`, claimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EvidenceOut
	for rows.Next() {
		var ev EvidenceOut
		if err := rows.Scan(&ev.ID, &ev.SourceID, &ev.Locator, &ev.Snippet); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (t *Tideglass) ExportProfile(ctx context.Context, opts ExportOptions) (ExportResult, error) {
	if opts.Format != "" && opts.Format != "tgz" {
		return ExportResult{}, fmt.Errorf("unsupported export format %q", opts.Format)
	}
	profile, err := t.Profile(ctx, ProfileOptions{IntentID: opts.IntentID, Kind: opts.Kind})
	if err != nil {
		return ExportResult{}, err
	}
	out := opts.Out
	if out == "" {
		out = filepath.Join(filepath.Dir(t.path), "tideglass-"+profile.Intent.Kind+"-"+time.Now().UTC().Format("20060102T150405Z")+".tgz")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return ExportResult{}, err
	}
	file, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ExportResult{}, err
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	records := 0
	if err := writeTarJSON(tw, "manifest.json", map[string]any{"schema_version": "tideglass.bundle.v1", "generated_at": now(), "intent": profile.Intent}); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records++
	if err := writeTarJSON(tw, "profile.json", profile); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records++
	if err := writeTarJSONL(tw, "claims.jsonl", profile.Claims); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records += len(profile.Claims)
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	if err := gz.Close(); err != nil {
		_ = file.Close()
		return ExportResult{}, err
	}
	if err := file.Close(); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Path: out, Format: "tgz", Records: records}, nil
}

func (t *Tideglass) Doctor(ctx context.Context) (DoctorResult, error) {
	sources, err := t.Sources(ctx, SourceOptions{Probe: true})
	if err != nil {
		return DoctorResult{}, err
	}
	checks := []CheckResult{
		{ID: "db.open", Status: "ok", Message: t.path},
		{ID: "schema.version", Status: "ok", Message: fmt.Sprintf("%d", schemaVersion)},
	}
	partial := false
	for _, source := range sources.Sources {
		if source.Health == "error" {
			partial = true
			checks = append(checks, CheckResult{ID: "source." + source.ID, Status: "warning", Message: source.LastError})
		}
		if source.Health == "partial" {
			partial = true
			checks = append(checks, CheckResult{ID: "source." + source.ID, Status: "warning", Message: source.LastError})
		}
	}
	state := "ok"
	if partial {
		state = "warning"
	}
	return DoctorResult{DBPath: t.path, Schema: schemaVersion, Sources: sources.Sources, Checks: checks, GeneratedAt: now(), OverallState: state}, nil
}

func (t *Tideglass) ensureIntent(ctx context.Context, kind, title string) (IntentOut, error) {
	kind = normalizeKind(kind, "")
	var existing IntentOut
	err := t.db.QueryRowContext(ctx, `select id,kind,title from intents where kind = ? and status = 'active' order by created_at desc limit 1`, kind).Scan(&existing.ID, &existing.Kind, &existing.Title)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return IntentOut{}, err
	}
	intent := IntentOut{ID: id("int"), Kind: kind, Title: title}
	if _, err := t.db.ExecContext(ctx, `insert into intents(id,kind,title,description,status,created_at,updated_at) values(?,?,?,?,?,?,?)`,
		intent.ID, intent.Kind, intent.Title, "", "active", now(), now()); err != nil {
		return IntentOut{}, err
	}
	return intent, nil
}

func (t *Tideglass) findIntent(ctx context.Context, intentID, kind string) (IntentOut, error) {
	var intent IntentOut
	if strings.TrimSpace(intentID) != "" {
		err := t.db.QueryRowContext(ctx, `select id,kind,title from intents where id = ?`, intentID).Scan(&intent.ID, &intent.Kind, &intent.Title)
		if err != nil {
			return IntentOut{}, err
		}
		return intent, nil
	}
	kind = normalizeKind(kind, "")
	err := t.db.QueryRowContext(ctx, `select id,kind,title from intents where kind = ? and status = 'active' order by updated_at desc limit 1`, kind).Scan(&intent.ID, &intent.Kind, &intent.Title)
	if err != nil {
		return IntentOut{}, err
	}
	return intent, nil
}

func (t *Tideglass) upsertSource(ctx context.Context, source SourceStatus) error {
	caps, _ := json.Marshal(source.Capabilities)
	counts, _ := json.Marshal(source.Counts)
	meta, _ := json.Marshal(source.Metadata)
	_, err := t.db.ExecContext(ctx, `
insert into sources(id,kind,label,locator,health,capabilities_json,counts_json,last_probe_at,last_error,metadata_json)
values(?,?,?,?,?,?,?,?,?,?)
on conflict(id) do update set
kind=excluded.kind,label=excluded.label,locator=excluded.locator,health=excluded.health,
capabilities_json=excluded.capabilities_json,counts_json=excluded.counts_json,last_probe_at=excluded.last_probe_at,
last_error=excluded.last_error,metadata_json=excluded.metadata_json`,
		source.ID, source.Kind, source.Label, source.Locator, source.Health, string(caps), string(counts), source.LastProbeAt, source.LastError, string(meta))
	return err
}

func (t *Tideglass) upsertArtifactEvidence(ctx context.Context, art artifact) (string, error) {
	if art.ContentHash == "" {
		art.ContentHash = hashText(art.Snippet)
	}
	if art.MetadataJSON == "" {
		art.MetadataJSON = "{}"
	}
	artifactID := "art_" + hashText(art.SourceID + "|" + art.ExternalID)[:24]
	_, err := t.db.ExecContext(ctx, `
insert into source_artifacts(id,source_id,external_id,artifact_kind,title,author_ref,created_at,updated_at,content_hash,metadata_json)
values(?,?,?,?,?,?,?,?,?,?)
on conflict(source_id, external_id) do update set
artifact_kind=excluded.artifact_kind,title=excluded.title,author_ref=excluded.author_ref,
created_at=excluded.created_at,updated_at=excluded.updated_at,content_hash=excluded.content_hash,metadata_json=excluded.metadata_json`,
		artifactID, art.SourceID, art.ExternalID, art.Kind, trimForSnippet(art.Title, 300), art.AuthorRef, art.CreatedAt, art.UpdatedAt, art.ContentHash, art.MetadataJSON)
	if err != nil {
		return "", err
	}
	evidenceID := "ev_" + hashText(artifactID + "|" + art.LocatorJSON + "|" + art.Snippet)[:24]
	if art.LocatorJSON == "" {
		locator, _ := json.Marshal(map[string]string{"external_id": art.ExternalID})
		art.LocatorJSON = string(locator)
	}
	_, err = t.db.ExecContext(ctx, `
insert into evidence(id,source_artifact_id,locator_json,snippet,snippet_hash,observed_at)
values(?,?,?,?,?,?)
on conflict(id) do update set snippet=excluded.snippet,snippet_hash=excluded.snippet_hash,observed_at=excluded.observed_at`,
		evidenceID, artifactID, art.LocatorJSON, trimForSnippet(art.Snippet, 1000), hashText(art.Snippet), now())
	if err != nil {
		return "", err
	}
	_, err = t.db.ExecContext(ctx, `insert or replace into evidence_fts(rowid,evidence_id,source_id,snippet) select rowid, ?, ?, ? from evidence where id = ?`,
		evidenceID, art.SourceID, trimForSnippet(art.Snippet, 1000), evidenceID)
	return evidenceID, err
}

func (t *Tideglass) insertClaim(ctx context.Context, intentID string, claim candidateClaim) (string, error) {
	claim.Value = strings.TrimSpace(claim.Value)
	if claim.Value == "" || claim.Kind == "" {
		return "", errors.New("claim kind and value are required")
	}
	sourceMode := claim.SourceMode
	if sourceMode == "" {
		sourceMode = "inferred"
	}
	claimID := id("clm")
	_, err := t.db.ExecContext(ctx, `
insert into claims(id,intent_id,subject,kind,value,normalized_value,polarity,scope,status,source_mode,confidence,created_at,updated_at)
values(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		claimID, intentID, "user", claim.Kind, claim.Value, normalizeText(claim.Value), "neutral", "", "active", sourceMode, claim.Confidence, now(), now())
	return claimID, err
}

func (t *Tideglass) loadClaims(ctx context.Context, intentID string) ([]ClaimOut, error) {
	rows, err := t.db.QueryContext(ctx, `
select c.id, c.kind,
       coalesce(json_extract(e.patch_json, '$.value'), c.value) as value,
       c.confidence, c.status, c.source_mode
from claims c
left join edits e on e.id = (
  select id from edits where claim_id = c.id order by created_at desc, rowid desc limit 1
)
where c.intent_id = ?
order by case when e.id is null then 0 else 1 end desc, c.confidence desc, c.created_at desc`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ClaimOut
	seen := map[string]bool{}
	for rows.Next() {
		var claim ClaimOut
		if err := rows.Scan(&claim.ID, &claim.Kind, &claim.Value, &claim.Confidence, &claim.Status, &claim.SourceMode); err != nil {
			return nil, err
		}
		key := claim.Kind + "\x00" + normalizeText(claim.Value)
		if singletonClaimKind(claim.Kind) {
			key = claim.Kind
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		claims = append(claims, claim)
	}
	return claims, rows.Err()
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
	var where []string
	var args []any
	for _, term := range terms[:minInt(len(terms), 5)] {
		where = append(where, `(lower(title) like ? or lower(coalesce(body_excerpt,'')) like ?)`)
		pattern := "%" + strings.ToLower(term) + "%"
		args = append(args, pattern, pattern)
	}
	args = append(args, limit)
	rows, err := ro.DB().QueryContext(ctx, `select id, title, coalesce(body_excerpt,''), html_url from threads where `+strings.Join(where, " or ")+` order by updated_at_gh desc limit ?`, args...)
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
where evidence_fts match ? and f.source_id = ?
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

func (t *Tideglass) ensureEvidenceSearch(ctx context.Context) error {
	var evidenceCount, ftsCount int64
	if err := t.db.QueryRowContext(ctx, `select count(*) from evidence`).Scan(&evidenceCount); err != nil {
		return err
	}
	if err := t.db.QueryRowContext(ctx, `select count(*) from evidence_fts`).Scan(&ftsCount); err != nil {
		return err
	}
	if evidenceCount == ftsCount {
		return nil
	}
	_, err := t.db.ExecContext(ctx, `
insert or replace into evidence_fts(rowid,evidence_id,source_id,snippet)
select e.rowid, e.id, sa.source_id, e.snippet
from evidence e
join source_artifacts sa on sa.id = e.source_artifact_id
where e.rowid not in (select rowid from evidence_fts)`)
	return err
}

func discoverStaticSources() []SourceStatus {
	return []SourceStatus{
		{ID: "gitcrawl", Kind: "crawl", Label: "GitHub", Locator: "~/.config/gitcrawl/gitcrawl.db", Health: "unknown"},
		{ID: "slacrawl", Kind: "crawl", Label: "Slack", Locator: "~/.slacrawl/slacrawl.db", Health: "unknown"},
		{ID: "discrawl", Kind: "crawl", Label: "Discord", Locator: "~/.discrawl/discrawl.db", Health: "unknown"},
		{ID: "notcrawl", Kind: "crawl", Label: "Notion", Locator: "~/.notcrawl/notcrawl.db", Health: "unknown"},
		{ID: "graincrawl", Kind: "crawl", Label: "Granola", Locator: "~/.config/graincrawl/graincrawl.db", Health: "unknown"},
		{ID: "codex", Kind: "import", Label: "Codex sessions", Locator: "~/.codex/sessions", Health: "unknown"},
		{ID: "chatgpt", Kind: "import", Label: "ChatGPT export", Locator: "", Health: "unknown"},
		{ID: "claude", Kind: "import", Label: "Claude export", Locator: "", Health: "unknown"},
	}
}

func discoverCrawlBar(ctx context.Context) ([]SourceStatus, error) {
	bin := "/Users/vincentkoc/.local/bin/crawlbar"
	if _, err := os.Stat(bin); err != nil {
		var lookErr error
		bin, lookErr = exec.LookPath("crawlbar")
		if lookErr != nil {
			return nil, err
		}
	}
	cmd := exec.CommandContext(ctx, bin, "apps", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Available   bool   `json:"available"`
		BinaryPath  string `json:"binary_path"`
		Enabled     bool   `json:"enabled"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, err
	}
	var out []SourceStatus
	for _, row := range rows {
		if !row.Available {
			continue
		}
		out = append(out, SourceStatus{
			ID:       row.ID,
			Kind:     "crawl",
			Label:    row.DisplayName,
			Health:   "available",
			Locator:  crawlbarDatabasePath(ctx, bin, row.ID),
			Metadata: map[string]string{"binary_path": row.BinaryPath, "enabled": fmt.Sprint(row.Enabled)},
		})
	}
	return out, nil
}

func crawlbarDatabasePath(ctx context.Context, bin, appID string) string {
	cmd := exec.CommandContext(ctx, bin, "status", "--app", appID, "--json")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	var rows []struct {
		DatabasePath string `json:"database_path"`
		Databases    []struct {
			Path      string `json:"path"`
			IsPrimary bool   `json:"is_primary"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(output, &rows); err != nil || len(rows) == 0 {
		return ""
	}
	if rows[0].DatabasePath != "" {
		return rows[0].DatabasePath
	}
	for _, database := range rows[0].Databases {
		if database.IsPrimary && database.Path != "" {
			return database.Path
		}
	}
	if len(rows[0].Databases) > 0 {
		return rows[0].Databases[0].Path
	}
	return ""
}

func probeSource(ctx context.Context, source SourceStatus) SourceStatus {
	source.LastProbeAt = now()
	source.Counts = map[string]int64{}
	switch source.ID {
	case "gitcrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"threads": "threads", "clusters": "cluster_groups"}, []string{"metadata", "text"})
	case "slacrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"messages": "messages", "channels": "channels", "users": "users"}, []string{"fts", "metadata", "text"})
	case "discrawl":
		out := probeSQLite(ctx, source, source.Locator, map[string]string{"messages": "messages", "members": "members", "embeddings": "message_embeddings"}, []string{"fts", "semantic", "metadata", "text"})
		if out.Health == "ok" {
			out.Health = "partial"
			out.LastError = "CrawlBar status reports schema version mismatch; direct read-only probe is usable"
		}
		return out
	case "notcrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"pages": "pages", "blocks": "blocks", "comments": "comments"}, []string{"fts", "metadata", "text"})
	case "graincrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"notes": "notes", "transcripts": "transcript_chunks"}, []string{"fts", "metadata", "text"})
	case "codex":
		count := int64(0)
		_ = filepath.WalkDir(expandHome(source.Locator), func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
				count++
			}
			return nil
		})
		source.Health = "ok"
		source.Capabilities = []string{"metadata", "text"}
		source.Counts = map[string]int64{"session_files": count}
		return source
	default:
		if source.Kind == "import" {
			source.Health = "unknown"
			source.Capabilities = []string{"metadata", "text"}
		}
		return source
	}
}

func probeSQLite(ctx context.Context, source SourceStatus, path string, tables map[string]string, caps []string) SourceStatus {
	source.Locator = path
	path = expandHome(path)
	if strings.TrimSpace(path) == "" {
		source.Health = "error"
		source.LastError = "database locator is empty"
		return source
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		source.Health = "error"
		source.LastError = err.Error()
		return source
	}
	defer ro.Close()
	source.Health = "ok"
	source.Capabilities = caps
	source.Counts = map[string]int64{}
	probeErrors := 0
	for label, table := range tables {
		var count int64
		query := `select count(*) from ` + store.QuoteIdent(table)
		if err := ro.DB().QueryRowContext(ctx, query).Scan(&count); err != nil {
			source.LastError = strings.TrimSpace(source.LastError + "; " + err.Error())
			probeErrors++
			continue
		}
		source.Counts[label] = count
	}
	if probeErrors > 0 {
		source.Health = "partial"
	}
	return source
}

func importCodex(root string, limit int) ([]artifact, []candidateClaim, error) {
	root = expandHome(root)
	type codexFile struct {
		Path    string
		ModTime time.Time
	}
	var files []codexFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		files = append(files, codexFile{Path: path, ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime.After(files[j].ModTime)
	})
	var artifacts []artifact
	for _, fileInfo := range files {
		path := fileInfo.Path
		if limit > 0 && len(artifacts) >= limit {
			break
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		reader := bufio.NewReaderSize(file, 1024*1024)
		lineNo := 0
		cwd := ""
		for {
			if limit > 0 && len(artifacts) >= limit {
				break
			}
			line, readErr := reader.ReadBytes('\n')
			if len(line) == 0 && readErr == io.EOF {
				break
			}
			if readErr != nil && readErr != io.EOF {
				_ = file.Close()
				return nil, nil, readErr
			}
			lineNo++
			if bytes.Contains(line, []byte(`"function_call_output"`)) ||
				bytes.Contains(line, []byte(`"tool_output"`)) {
				if readErr == io.EOF {
					break
				}
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal(line, &obj); err != nil {
				if readErr == io.EOF {
					break
				}
				continue
			}
			if typ, _ := obj["type"].(string); typ == "session_meta" {
				if payload, _ := obj["payload"].(map[string]any); payload != nil {
					cwd, _ = payload["cwd"].(string)
				}
				if readErr == io.EOF {
					break
				}
				continue
			}
			text := codexEventText(obj)
			if len(text) < 40 {
				if readErr == io.EOF {
					break
				}
				continue
			}
			meta, _ := json.Marshal(map[string]any{"path": path, "line": lineNo, "cwd": cwd})
			locator, _ := json.Marshal(map[string]any{"path": path, "line": lineNo})
			title := filepath.Base(path)
			if cwd != "" {
				title = cwd
			}
			artifacts = append(artifacts, artifact{
				SourceID:     "codex",
				ExternalID:   fmt.Sprintf("%s:%d", path, lineNo),
				Kind:         "codex_event",
				Title:        title,
				ContentHash:  hashBytes(line),
				MetadataJSON: string(meta),
				Snippet:      trimForSnippet(text, 1000),
				LocatorJSON:  string(locator),
			})
			if readErr == io.EOF {
				break
			}
		}
		_ = file.Close()
	}
	return artifacts, nil, nil
}

func importAssistantExport(kind, path string, limit int) ([]artifact, []candidateClaim, error) {
	path = expandHome(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	var blobs []namedBlob
	if info.IsDir() {
		blobs, err = readJSONFiles(path, limit)
	} else if strings.HasSuffix(strings.ToLower(path), ".zip") {
		blobs, err = readZipJSON(path, limit)
	} else {
		data, readErr := readFileLimited(path)
		err = readErr
		if err == nil {
			blobs = []namedBlob{{Name: filepath.Base(path), Data: data}}
		}
	}
	if err != nil {
		return nil, nil, err
	}
	var artifacts []artifact
	var claims []candidateClaim
	for _, blob := range blobs {
		var value any
		if err := json.Unmarshal(blob.Data, &value); err != nil {
			continue
		}
		texts := collectTexts(value, nil)
		for index, text := range texts {
			if limit > 0 && len(artifacts) >= limit {
				break
			}
			text = trimForSnippet(text, 1000)
			if len(text) < 40 {
				continue
			}
			locator, _ := json.Marshal(map[string]any{"file": blob.Name, "index": index})
			artifacts = append(artifacts, artifact{
				SourceID:     kind,
				ExternalID:   fmt.Sprintf("%s:%d", blob.Name, index),
				Kind:         "assistant_export_text",
				Title:        blob.Name,
				ContentHash:  hashText(text),
				MetadataJSON: "{}",
				Snippet:      text,
				LocatorJSON:  string(locator),
			})
		}
	}
	return artifacts, claims, nil
}

type namedBlob struct {
	Name string
	Data []byte
}

func readZipJSON(path string, limit int) ([]namedBlob, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var out []namedBlob
	for _, file := range zr.File {
		if limit > 0 && len(out) >= limit {
			break
		}
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".json") {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			continue
		}
		data, err := readAllLimited(rc, assistantJSONLimit, file.Name)
		_ = rc.Close()
		if err == nil {
			out = append(out, namedBlob{Name: file.Name, Data: data})
		} else {
			return nil, err
		}
	}
	return out, nil
}

func readFileLimited(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readAllLimited(file, assistantJSONLimit, path)
}

func readAllLimited(reader io.Reader, limit int64, name string) ([]byte, error) {
	var buf bytes.Buffer
	written, err := io.Copy(&buf, io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if written > limit {
		return nil, fmt.Errorf("%s is too large to import safely: %d bytes exceeds %d byte limit", name, written, limit)
	}
	return buf.Bytes(), nil
}

func readJSONFiles(root string, limit int) ([]namedBlob, error) {
	var out []namedBlob
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}
		if limit > 0 && len(out) >= limit {
			return filepath.SkipAll
		}
		data, err := readFileLimited(path)
		if err == nil {
			out = append(out, namedBlob{Name: path, Data: data})
		} else {
			return err
		}
		return nil
	})
	return out, err
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
	switch kind {
	case "work.project.start", "work.release", "agent.delegation":
		if strings.Contains(all, "live") || strings.Contains(all, "evidence") || strings.Contains(all, "proof") || strings.Contains(all, "exact sha") {
			add("preference.agent.evidence", "Prefer live, concrete evidence over stale assumptions; include exact proof such as command output, CI state, or landed SHA when relevant.", 0.88, "inferred", findEvidence(evidence, "live", "evidence", "proof", "exact", "sha"))
		}
		if strings.Contains(all, "test") || strings.Contains(all, "validate") || strings.Contains(all, "check") || strings.Contains(all, "ci") {
			add("preference.project.validation", "Prefer scoped verification first, then broader gates when risk or user request requires it.", 0.82, "inferred", findEvidence(evidence, "test", "validate", "check", "ci", "proof"))
		}
		if strings.Contains(all, "worktree") || strings.Contains(all, "gwt") || strings.Contains(all, "branch") {
			add("preference.project.branching", "Prefer fresh, scoped worktrees and explicit branch/worktree state for implementation lanes.", 0.8, "inferred", findEvidence(evidence, "worktree", "gwt", "branch"))
		}
		if strings.Contains(all, "concise") || strings.Contains(all, "terse") || strings.Contains(all, "no fluff") {
			add("preference.agent.communication", "Use terse, operational updates with concrete state and next action; avoid filler.", 0.86, "inferred", findEvidence(evidence, "concise", "terse", "fluff", "status"))
		}
		if strings.Contains(all, "kill") || strings.Contains(all, "process") || strings.Contains(all, "tmux") {
			add("boundary.agent.process_kill", "Do not kill broad Codex, agent, tmux, SSH, mosh, or terminal processes without explicit current-turn scope.", 0.9, "inferred", findEvidence(evidence, "kill", "process", "tmux", "mosh"))
		}
		if strings.Contains(all, "push") || strings.Contains(all, "merge") || strings.Contains(all, "external") || strings.Contains(all, "land") {
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

func unresolvedQuestions(kind string, claims []ClaimOut) []string {
	have := map[string]bool{}
	for _, claim := range claims {
		have[claim.Kind] = true
	}
	need := map[string][]string{
		"social.dinner": {
			"preference.food.allergy:Any allergies or hard dietary restrictions?",
			"preference.food.budget:What dinner budget range is comfortable?",
			"preference.social.group_size:What group size feels good with strangers?",
			"boundary.social.topic:Any topics to avoid with strangers?",
		},
		"social.dating": {
			"preference.dating.relationship_goal:What relationship goal should be explicit?",
			"boundary.dating.dealbreaker:What dealbreakers should never be inferred?",
			"boundary.dating.safety:What safety boundaries should be explicit?",
		},
		"work.new_job": {
			"preference.work.communication:What communication cadence should a new team know?",
			"boundary.work.focus_time:What focus-time boundaries matter?",
		},
		"work.project.start": {
			"boundary.project.no_go:Any project-specific no-go areas?",
			"context.project.related_repos:Which repos are definitely in scope?",
		},
	}
	rows := need[kind]
	if len(rows) == 0 && strings.HasPrefix(kind, "work.") {
		rows = need["work.project.start"]
	}
	var out []string
	for _, row := range rows {
		parts := strings.SplitN(row, ":", 2)
		if len(parts) == 2 && !have[parts[0]] {
			out = append(out, parts[1])
		}
	}
	return out
}

func singletonClaimKind(kind string) bool {
	if kind == "preference.agent.imported_memory" {
		return false
	}
	return strings.HasPrefix(kind, "preference.agent.") ||
		strings.HasPrefix(kind, "boundary.agent.") ||
		strings.HasPrefix(kind, "preference.project.") ||
		strings.HasPrefix(kind, "boundary.project.") ||
		strings.HasPrefix(kind, "preference.work.") ||
		strings.HasPrefix(kind, "boundary.work.")
}

func sourceAllowed(kind, sourceID string) bool {
	switch kind {
	case "social.dinner", "social.dating":
		switch sourceID {
		case "slacrawl", "discrawl", "chatgpt", "claude":
			return true
		default:
			return false
		}
	case "work.project.start", "work.release", "agent.delegation", "work.new_job":
		switch sourceID {
		case "codex", "gitcrawl", "notcrawl", "graincrawl", "slacrawl", "discrawl", "chatgpt", "claude":
			return true
		default:
			return false
		}
	default:
		return true
	}
}

func renderAgentContext(intent IntentOut, claims []ClaimOut, unresolved []string, budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "intent: %s (%s)\n", intent.Title, intent.Kind)
	if len(claims) > 0 {
		b.WriteString("\npreferences and constraints:\n")
		for _, claim := range claims {
			fmt.Fprintf(&b, "- %s: %s (confidence %.2f)\n", claim.Kind, claim.Value, claim.Confidence)
		}
	}
	if len(unresolved) > 0 {
		b.WriteString("\nunresolved:\n")
		for _, question := range unresolved {
			fmt.Fprintf(&b, "- %s\n", question)
		}
	}
	text := strings.TrimSpace(b.String())
	if budget > 0 && len(text) > budget {
		return strings.TrimSpace(text[:budget]) + "\n..."
	}
	return text
}

func Print(value any, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	switch v := value.(type) {
	case SourceList:
		for _, source := range v.Sources {
			fmt.Printf("%-10s %-8s %-7s %s\n", source.ID, source.Kind, source.Health, source.Locator)
			if source.LastError != "" {
				fmt.Printf("  warning: %s\n", source.LastError)
			}
		}
	case IngestResult:
		fmt.Printf("ingested %s: artifacts=%d evidence=%d claims=%d\n", v.Kind, v.Artifacts, v.Evidence, v.Claims)
	case AskResult:
		fmt.Printf("intent: %s (%s)\n", v.Intent.Title, v.Intent.Kind)
		fmt.Println("claims:")
		for _, claim := range v.Claims {
			fmt.Printf("- [%s] %s (%.2f)\n", claim.Kind, claim.Value, claim.Confidence)
		}
		if len(v.UnresolvedQuestions) > 0 {
			fmt.Println("unresolved:")
			for _, question := range v.UnresolvedQuestions {
				fmt.Printf("- %s\n", question)
			}
		}
		fmt.Println("source coverage:")
		for _, coverage := range v.SourceCoverage {
			fmt.Printf("- %s: %s hits=%d", coverage.SourceID, coverage.Health, coverage.Hits)
			if coverage.Error != "" {
				fmt.Printf(" warning=%s", coverage.Error)
			}
			fmt.Println()
		}
	case ProfileResult:
		if v.Text != "" {
			fmt.Println(v.Text)
			return nil
		}
		fmt.Printf("intent: %s (%s)\n", v.Intent.Title, v.Intent.Kind)
		for _, claim := range v.Claims {
			fmt.Printf("- [%s] %s (%.2f)\n", claim.Kind, claim.Value, claim.Confidence)
		}
	case EditResult:
		fmt.Printf("edited %s -> %s\n", v.ClaimID, v.Value)
	case ExportResult:
		fmt.Printf("exported %s records=%d\n", v.Path, v.Records)
	case DoctorResult:
		fmt.Printf("doctor: %s db=%s schema=%d\n", v.OverallState, v.DBPath, v.Schema)
		for _, check := range v.Checks {
			fmt.Printf("- %s: %s %s\n", check.ID, check.Status, check.Message)
		}
	case []EvidenceOut:
		for _, ev := range v {
			fmt.Printf("- %s %s %s\n", ev.SourceID, ev.ID, ev.Snippet)
		}
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	return nil
}

func PrintText(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format, args...)
	return err
}

func defaultDBPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return expandHome(path), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tideglass", "tideglass.db"), nil
}

func normalizeKind(kind, query string) string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind != "" {
		return kind
	}
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "dinner"):
		return "social.dinner"
	case strings.Contains(q, "dating") || hasAnyWord(q, "date") || hasDatingPhrase(q):
		return "social.dating"
	case strings.Contains(q, "new job"):
		return "work.new_job"
	case strings.Contains(q, "agent"):
		return "agent.delegation"
	default:
		return "work.project.start"
	}
}

func hasAnyWord(text string, targets ...string) bool {
	targetSet := map[string]bool{}
	for _, target := range targets {
		targetSet[target] = true
	}
	for _, word := range words(text) {
		if targetSet[word] {
			return true
		}
	}
	return false
}

func hasDatingPhrase(text string) bool {
	tokens := wordTokens(text)
	return hasTokenSequence(tokens, "first", "dates") ||
		hasTokenSequence(tokens, "better", "dates") ||
		hasTokenSequence(tokens, "date", "ideas") ||
		hasTokenSequence(tokens, "dating", "ideas")
}

func hasTokenSequence(tokens []string, sequence ...string) bool {
	if len(sequence) == 0 || len(sequence) > len(tokens) {
		return false
	}
	for i := 0; i <= len(tokens)-len(sequence); i++ {
		matched := true
		for j, token := range sequence {
			if tokens[i+j] != token {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func titleForKind(kind string) string {
	switch kind {
	case "social.dinner":
		return "Dinner with strangers"
	case "social.dating":
		return "Dating preferences"
	case "work.new_job":
		return "New job onboarding"
	case "agent.delegation":
		return "Agent delegation"
	default:
		return "Project start"
	}
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func id(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func hashText(text string) string {
	return hashBytes([]byte(text))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeText(text string) string {
	return strings.Join(words(text), " ")
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
		if len(word) < 3 || stopword(word) {
			continue
		}
		out = append(out, word)
	}
	return out
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

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func mergeStringMaps(left, right map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range left {
		out[k] = v
	}
	for k, v := range right {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func writeTarJSON(tw *tar.Writer, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func writeTarJSONL[T any](tw *tar.Writer, name string, rows []T) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(buf.Len()), ModTime: time.Now()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(buf.Bytes())
	return err
}

func codexEventText(obj map[string]any) string {
	typ, _ := obj["type"].(string)
	payload, _ := obj["payload"].(map[string]any)
	if payload == nil {
		return ""
	}
	var text string
	switch typ {
	case "event_msg":
		msgType, _ := payload["type"].(string)
		if msgType != "user_message" && msgType != "agent_message" {
			return ""
		}
		text, _ = payload["message"].(string)
	case "response_item":
		itemType, _ := payload["type"].(string)
		if itemType != "message" {
			return ""
		}
		role, _ := payload["role"].(string)
		if role != "user" && role != "assistant" {
			return ""
		}
		text = strings.Join(collectTexts(payload["content"], nil), "\n")
	default:
		return ""
	}
	text = trimForSnippet(text, 1000)
	lower := strings.ToLower(text)
	if strings.Contains(lower, "agеnts.md instructions") ||
		strings.Contains(lower, "agents.md instructions") ||
		strings.Contains(lower, "codex_internal_context") ||
		strings.Contains(lower, "base_instructions") ||
		strings.Contains(lower, "<instructions>") {
		return ""
	}
	if looksLikePathOnly(text) {
		return ""
	}
	return text
}

func collectTexts(value any, out []string) []string {
	switch v := value.(type) {
	case string:
		if usefulString(v) {
			out = append(out, v)
		}
	case []any:
		for _, item := range v {
			out = collectTexts(item, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := v[key]
			lower := strings.ToLower(key)
			if noisyExportKey(lower) {
				continue
			}
			if lower == "content" || lower == "text" || lower == "message" || lower == "memory" || lower == "title" || lower == "summary" || lower == "name" {
				out = collectTexts(item, out)
				continue
			}
			if lower == "payload" || lower == "items" || lower == "parts" || lower == "messages" || lower == "chat_messages" || lower == "mapping" || lower == "children" {
				out = collectTexts(item, out)
				continue
			}
			switch item.(type) {
			case map[string]any, []any:
				out = collectTexts(item, out)
			}
		}
	}
	return out
}

func noisyExportKey(key string) bool {
	switch key {
	case "id", "uuid", "conversation_id", "parent", "parent_id", "children_ids", "create_time", "update_time", "timestamp", "metadata", "author", "recipient", "status", "end_turn", "weight", "model", "finish_details":
		return true
	default:
		return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids")
	}
}

func usefulString(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) < 20 {
		return false
	}
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 12
}

func looksLikePathOnly(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "/Users/") && !strings.Contains(text, " ")
}

func looksLikeMemory(name, text string) bool {
	lower := strings.ToLower(name + " " + text)
	return strings.Contains(lower, "memory") || strings.Contains(lower, "preference") || strings.Contains(lower, "remember")
}

const schemaSQL = `
create table if not exists intents (
  id text primary key,
  kind text not null,
  title text not null,
  description text not null default '',
  status text not null default 'active',
  created_at text not null,
  updated_at text not null,
  source_query_id text
);

create table if not exists queries (
  id text primary key,
  intent_id text not null references intents(id),
  raw_query text not null,
  normalized_query text not null,
  kind_hint text not null default '',
  expansion_version text not null,
  created_at text not null
);

create table if not exists query_expansions (
  id text primary key,
  query_id text not null references queries(id),
  question text not null,
  probe text not null,
  target_claim_kind text not null,
  priority integer not null default 0,
  source_policy text not null default 'auto',
  created_at text not null
);

create table if not exists sources (
  id text primary key,
  kind text not null,
  label text not null,
  locator text not null,
  health text not null default 'unknown',
  capabilities_json text not null default '[]',
  counts_json text not null default '{}',
  last_probe_at text,
  last_error text not null default '',
  metadata_json text not null default '{}'
);

create table if not exists source_artifacts (
  id text primary key,
  source_id text not null references sources(id),
  external_id text not null,
  artifact_kind text not null,
  title text not null default '',
  author_ref text not null default '',
  created_at text,
  updated_at text,
  content_hash text not null,
  metadata_json text not null default '{}',
  unique(source_id, external_id)
);

create table if not exists evidence (
  id text primary key,
  source_artifact_id text not null references source_artifacts(id),
  locator_json text not null,
  snippet text not null default '',
  snippet_hash text not null default '',
  observed_at text not null
);

create virtual table if not exists evidence_fts using fts5(
  evidence_id unindexed,
  source_id unindexed,
  snippet
);

create table if not exists claims (
  id text primary key,
  intent_id text not null references intents(id),
  subject text not null default 'user',
  kind text not null,
  value text not null,
  normalized_value text not null default '',
  polarity text not null default 'neutral',
  scope text not null default '',
  status text not null default 'active',
  source_mode text not null,
  confidence real not null,
  valid_from text,
  valid_until text,
  created_at text not null,
  updated_at text not null
);

create table if not exists claim_evidence (
  claim_id text not null references claims(id),
  evidence_id text not null references evidence(id),
  role text not null default 'supporting',
  primary key (claim_id, evidence_id)
);

create table if not exists claim_edges (
  id text primary key,
  from_claim_id text not null references claims(id),
  to_claim_id text not null references claims(id),
  relation text not null,
  confidence real not null,
  created_at text not null
);

create table if not exists answers (
  id text primary key,
  intent_id text not null references intents(id),
  query_expansion_id text references query_expansions(id),
  answer text not null,
  answer_mode text not null default 'user',
  created_at text not null
);

create table if not exists edits (
  id text primary key,
  claim_id text not null references claims(id),
  operation text not null,
  patch_json text not null,
  reason text not null default '',
  created_at text not null
);

create table if not exists embeddings (
  id text primary key,
  owner_kind text not null,
  owner_id text not null,
  model text not null,
  dimensions integer not null,
  vector blob not null,
  content_hash text not null,
  created_at text not null,
  unique(owner_kind, owner_id, model)
);

create table if not exists runs (
  id text primary key,
  command text not null,
  input_json text not null default '{}',
  output_json text not null default '{}',
  status text not null,
  started_at text not null,
  finished_at text
);

create index if not exists idx_claims_intent_kind on claims(intent_id, kind);
create index if not exists idx_claims_scope on claims(scope);
create index if not exists idx_evidence_artifact on evidence(source_artifact_id);
create index if not exists idx_embeddings_owner on embeddings(owner_kind, owner_id);
`
