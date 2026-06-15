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
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/openclaw/crawlkit/store"
	"github.com/openclaw/crawlkit/vector"
)

const schemaVersion = 3

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

type ReviewOptions struct {
	ClaimID string
	Action  string
	Reason  string
}

type ExportOptions struct {
	IntentID string
	Kind     string
	Format   string
	Out      string
}

type ResolveOptions struct {
	Request     IntentRequestEnvelope
	AllowAction bool
	NoPersist   bool
}

type IntentActor struct {
	Type         string   `json:"type,omitempty"`
	ID           string   `json:"id,omitempty"`
	Session      string   `json:"session,omitempty"`
	TrustTier    string   `json:"trust_tier,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type IntentTask struct {
	Goal     string `json:"goal,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Stakes   string `json:"stakes,omitempty"`
	Autonomy string `json:"autonomy,omitempty"`
	Deadline string `json:"deadline,omitempty"`
}

type IntentAudience struct {
	Type      string   `json:"type,omitempty"`
	ID        string   `json:"id,omitempty"`
	ShareWith []string `json:"share_with,omitempty"`
}

func (audience *IntentAudience) UnmarshalJSON(data []byte) error {
	var label string
	if err := json.Unmarshal(data, &label); err == nil {
		audience.Type = strings.TrimSpace(label)
		audience.ID = ""
		audience.ShareWith = nil
		return nil
	}
	type audienceAlias IntentAudience
	var decoded audienceAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*audience = IntentAudience(decoded)
	return nil
}

func (audience IntentAudience) Label() string {
	if strings.TrimSpace(audience.ID) != "" {
		return strings.TrimSpace(audience.ID)
	}
	if strings.TrimSpace(audience.Type) != "" {
		return strings.TrimSpace(audience.Type)
	}
	return "agent"
}

type IntentContract struct {
	Output          string   `json:"output,omitempty"`
	RequiredSlots   []string `json:"required_slots,omitempty"`
	OptionalSlots   []string `json:"optional_slots,omitempty"`
	ConfidenceFloor float64  `json:"confidence_floor,omitempty"`
	AskStrategy     string   `json:"ask_strategy,omitempty"`
}

type IntentProof struct {
	Hash         string   `json:"hash,omitempty"`
	Commitments  string   `json:"commitments,omitempty"`
	ZKPredicates []string `json:"zk_predicates,omitempty"`
}

type IntentFreshness struct {
	MaxAge                     string `json:"max_age,omitempty"`
	RequireReviewed            *bool  `json:"require_reviewed,omitempty"`
	AcceptInferredForQuestions bool   `json:"accept_inferred_for_questions,omitempty"`
}

type IntentDisclosure struct {
	Mode             string `json:"mode,omitempty"`
	AllowValues      *bool  `json:"allow_values,omitempty"`
	AllowEvidence    bool   `json:"allow_evidence,omitempty"`
	AllowSensitive   bool   `json:"allow_sensitive,omitempty"`
	AllowCommitments *bool  `json:"allow_commitments,omitempty"`
}

type IntentRequestEnvelope struct {
	SchemaVersion string           `json:"schema_version,omitempty"`
	RequestID     string           `json:"request_id,omitempty"`
	URI           string           `json:"uri"`
	Actor         IntentActor      `json:"actor,omitempty"`
	Task          IntentTask       `json:"task,omitempty"`
	Purpose       string           `json:"purpose,omitempty"`
	Audience      IntentAudience   `json:"audience,omitempty"`
	Freshness     IntentFreshness  `json:"freshness,omitempty"`
	Disclosure    IntentDisclosure `json:"disclosure,omitempty"`
	Contract      IntentContract   `json:"contract,omitempty"`
	Context       map[string]any   `json:"context,omitempty"`
	Proof         IntentProof      `json:"proof,omitempty"`
}

type IntentResponseEnvelope struct {
	SchemaVersion string                `json:"schema_version"`
	RequestID     string                `json:"request_id,omitempty"`
	URI           string                `json:"uri"`
	ResolvedURI   string                `json:"resolved_uri"`
	Resource      IntentResource        `json:"resource,omitempty"`
	Intent        IntentOut             `json:"intent"`
	Status        string                `json:"status"`
	Decision      IntentDecision        `json:"decision,omitempty"`
	ProfileHash   string                `json:"profile_hash,omitempty"`
	SnapshotID    string                `json:"snapshot_id,omitempty"`
	Claims        []IntentClaimEnvelope `json:"claims"`
	Unresolved    []IntentQuestion      `json:"unresolved"`
	Policy        IntentPolicyEnvelope  `json:"policy"`
	Commitments   IntentCommitments     `json:"commitments,omitempty"`
	Links         map[string]string     `json:"links"`
}

type IntentResource struct {
	Type    string `json:"type,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version,omitempty"`
}

type IntentDecision struct {
	MayAct          bool   `json:"may_act"`
	Reason          string `json:"reason,omitempty"`
	Autonomy        string `json:"autonomy,omitempty"`
	NeedsUserAnswer bool   `json:"needs_user_answer"`
}

type IntentCommitments struct {
	ResponseHash string `json:"response_hash,omitempty"`
	ClaimRoot    string `json:"claim_root,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	Algorithm    string `json:"algorithm,omitempty"`
}

type IntentClaimEnvelope struct {
	ID          string   `json:"id,omitempty"`
	Kind        string   `json:"kind"`
	Value       string   `json:"value,omitempty"`
	Confidence  float64  `json:"confidence,omitempty"`
	Status      string   `json:"status"`
	SourceMode  string   `json:"source_mode,omitempty"`
	Sensitivity string   `json:"sensitivity,omitempty"`
	FreshAt     string   `json:"fresh_at,omitempty"`
	Commitment  string   `json:"commitment,omitempty"`
	Evidence    []string `json:"evidence,omitempty"`
}

type IntentQuestion struct {
	Kind         string `json:"kind"`
	Slot         string `json:"slot,omitempty"`
	Question     string `json:"question"`
	Priority     string `json:"priority"`
	BlocksAction bool   `json:"blocks_action,omitempty"`
	AnswerType   string `json:"answer_type,omitempty"`
}

type IntentPolicyEnvelope struct {
	Audience           string   `json:"audience"`
	DisclosureMode     string   `json:"disclosure_mode"`
	MayAct             bool     `json:"may_act"`
	NeedsUserAnswer    bool     `json:"needs_user_answer"`
	SafeToShare        []string `json:"safe_to_share"`
	Redacted           []string `json:"redacted"`
	Freshness          string   `json:"freshness,omitempty"`
	CapabilityRequired []string `json:"capability_required,omitempty"`
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
	UpdatedAt  string   `json:"updated_at,omitempty"`
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
	Evidence            []EvidenceOut    `json:"evidence,omitempty"`
	UnresolvedQuestions []string         `json:"unresolved_questions,omitempty"`
	SourceCoverage      []SourceCoverage `json:"source_coverage,omitempty"`
	Text                string           `json:"text,omitempty"`
}

type EditResult struct {
	EditID  string `json:"edit_id"`
	ClaimID string `json:"claim_id"`
	Value   string `json:"value"`
}

type ReviewResult struct {
	ClaimID string `json:"claim_id"`
	Action  string `json:"action"`
	Status  string `json:"status"`
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

var wordRE = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*`)

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
	if err := tg.ensureIntentRequestSchema(ctx); err != nil {
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
		source, err := t.mergePersistedSource(ctx, source)
		if err != nil {
			return SourceList{}, err
		}
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
			existing, err := t.claimExistsForEvidence(ctx, evidenceID, "preference.agent.imported_memory")
			if err != nil {
				return IngestResult{}, err
			}
			if existing {
				continue
			}
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
	actionGateClaims, err := t.loadActionGateClaims(ctx, intent.ID, intent.Kind, nil)
	if err != nil {
		return ProfileResult{}, err
	}
	claims = mergeClaimOuts(claims, actionGateClaims)
	evidence := []EvidenceOut(nil)
	claims, evidence, err = t.attachClaimEvidence(ctx, claims)
	if err != nil {
		return ProfileResult{}, err
	}
	unresolved := unresolvedQuestions(intent.Kind, claims)
	result := ProfileResult{Intent: intent, ForAgent: opts.ForAgent, Claims: claims, Evidence: evidence, UnresolvedQuestions: unresolved}
	if opts.ForAgent != "" || opts.Budget > 0 {
		result.Text = renderAgentContext(intent, claims, unresolved, opts.Budget)
	}
	return result, nil
}

func (t *Tideglass) ResolveIntent(ctx context.Context, opts ResolveOptions) (IntentResponseEnvelope, error) {
	if opts.NoPersist && opts.AllowAction {
		return IntentResponseEnvelope{}, errors.New("action authorization requires persisted audit")
	}
	request := opts.Request
	request.URI = strings.TrimSpace(request.URI)
	kind, audienceFromURI, err := parseIntentURI(request.URI)
	if err != nil {
		return IntentResponseEnvelope{}, err
	}
	if request.Audience.Label() == "agent" && audienceFromURI != "" {
		request.Audience = IntentAudience{Type: audienceFromURI}
	}
	request = normalizeIntentRequest(request)
	if err := validateIntentRequest(request); err != nil {
		return IntentResponseEnvelope{}, err
	}
	requestID := id("req")
	if request.Context == nil {
		request.Context = map[string]any{}
	}
	if strings.TrimSpace(request.RequestID) != "" {
		request.Context["client_request_id"] = strings.TrimSpace(request.RequestID)
	}
	request.Context["server_authority"] = map[string]any{
		"allow_action": opts.AllowAction,
	}
	request.RequestID = requestID
	if !opts.NoPersist {
		var err error
		requestID, err = t.persistIntentRequest(ctx, request, requestID)
		if err != nil {
			return IntentResponseEnvelope{}, err
		}
	}
	intent, foundIntent, err := t.findIntentForResolve(ctx, kind)
	if err != nil {
		return IntentResponseEnvelope{}, err
	}
	var claims []ClaimOut
	var policyFailedClaims []ClaimOut
	var unresolved []IntentQuestion
	requireReviewed := true
	if request.Freshness.RequireReviewed != nil {
		requireReviewed = *request.Freshness.RequireReviewed
	}
	maxAge, err := parseMaxAge(request.Freshness.MaxAge)
	if err != nil {
		return IntentResponseEnvelope{}, err
	}
	if foundIntent {
		loaded, err := t.loadClaims(ctx, intent.ID)
		if err != nil {
			return IntentResponseEnvelope{}, err
		}
		loaded, _, err = t.attachClaimEvidence(ctx, loaded)
		if err != nil {
			return IntentResponseEnvelope{}, err
		}
		claims = eligibleClaimsForRequest(loaded, requireReviewed, maxAge)
		policyFailedClaims = policyFailingClaims(loaded, claims)
		if request.Task.Mode == "act_gate" {
			actionGateClaims, err := t.loadActionGateClaims(ctx, intent.ID, kind, request.Contract.RequiredSlots)
			if err != nil {
				return IntentResponseEnvelope{}, err
			}
			policyFailedClaims = mergeClaimOuts(policyFailedClaims, actionGatePolicyFailingClaims(kind, actionGateClaims, request, maxAge))
		}
		slotClaims := claimsForSlotSatisfaction(kind, loaded, request, maxAge)
		unresolved = unresolvedIntentQuestions(intent.Kind, slotClaims)
		unresolved = addRequiredSlotQuestions(unresolved, slotClaims, request.Contract.RequiredSlots)
		if request.Task.Mode == "act_gate" && !hasActionGateConstraint(kind, claims, request, maxAge) {
			unresolved = append(unresolved, questionForSlot("policy.action.constraints", "critical", true))
		}
	} else {
		intent = IntentOut{Kind: kind, Title: titleForKind(kind)}
		unresolved = unresolvedIntentQuestions(kind, nil)
		unresolved = addRequiredSlotQuestions(unresolved, nil, request.Contract.RequiredSlots)
	}
	commitments := claimCommitmentsForClaims(claims)
	filteredClaims, policy := applyIntentPolicy(kind, claims, unresolved, request, opts.AllowAction)
	filteredClaims = addClaimCommitments(filteredClaims, request, commitments)
	redactedBlocking := redactedBlockingQuestions(kind, policy.Redacted, request.Contract.RequiredSlots, unresolved)
	if len(redactedBlocking) > 0 {
		unresolved = append(unresolved, redactedBlocking...)
	}
	var policyFailedBlocking []IntentQuestion
	if request.Task.Mode == "act_gate" {
		policyFailedBlocking = policyFailedBlockingQuestions(kind, policyFailedClaims, request.Contract.RequiredSlots, unresolved)
		if len(policyFailedBlocking) > 0 {
			unresolved = append(unresolved, policyFailedBlocking...)
		}
	}
	if request.Task.Mode == "act_gate" && len(claims) == 0 && len(policyFailedClaims) > 0 {
		unresolved = append(unresolved, questionForSlot("policy.claim.review_or_freshness", "critical", true))
	}
	if hasCriticalUnresolved(redactedBlocking) || hasRedactedCriticalClaim(kind, policy.Redacted) {
		policy.NeedsUserAnswer = true
		policy.MayAct = false
	}
	if request.Task.Mode == "act_gate" && len(policyFailedBlocking) > 0 {
		policy.NeedsUserAnswer = true
		policy.MayAct = false
	}
	if request.Task.Mode == "act_gate" && len(claims) == 0 && len(policyFailedClaims) > 0 {
		policy.NeedsUserAnswer = true
		policy.MayAct = false
	}
	if !foundIntent {
		policy.NeedsUserAnswer = true
		policy.MayAct = false
	}
	status := "ready"
	if !foundIntent || policy.NeedsUserAnswer {
		status = "partial"
	}
	if foundIntent && request.Task.Mode == "act_gate" && !policy.MayAct {
		status = "partial"
	}
	if !foundIntent {
		status = "missing"
	}
	decision := IntentDecision{
		MayAct:          policy.MayAct,
		Reason:          decisionReason(foundIntent, policy, request.Task.Mode, request.Task.Autonomy, opts.AllowAction),
		Autonomy:        request.Task.Autonomy,
		NeedsUserAnswer: policy.NeedsUserAnswer,
	}
	response := IntentResponseEnvelope{
		SchemaVersion: "tideglass.intent_response.v2",
		RequestID:     requestID,
		URI:           "tideglass://v1/intent/" + kind + "/current",
		ResolvedURI:   "tideglass://v1/profile/me/" + kind + "/current",
		Resource:      IntentResource{Type: "intent", Kind: kind, Title: intent.Title, Version: "current"},
		Intent:        intent,
		Status:        status,
		Decision:      decision,
		Claims:        filteredClaims,
		Unresolved:    unresolved,
		Policy:        policy,
		Links: map[string]string{
			"profile":    "tideglass://v1/profile/me/" + kind + "/current",
			"disclosure": "tideglass://disclosure/" + kind + "/" + request.Audience.Label(),
		},
	}
	if allowCommitments(request.Disclosure) {
		response.Commitments = IntentCommitments{
			ClaimRoot: claimRoot(filteredClaims),
			Algorithm: "canonical-json-sha256-v1",
		}
	}
	response.ProfileHash = profileHash(response)
	if allowCommitments(request.Disclosure) {
		response.Commitments.ResponseHash = response.ProfileHash
	}
	if !opts.NoPersist {
		response.SnapshotID, err = t.persistProfileSnapshot(ctx, requestID, response)
		if err != nil {
			return IntentResponseEnvelope{}, err
		}
		if hasCommitments(response.Commitments) {
			response.Commitments.SnapshotID = response.SnapshotID
		}
	}
	return response, nil
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
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return EditResult{}, err
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
		editID, opts.ClaimID, "supersede", string(patch), opts.Reason, now()); err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `update claims set status = 'active', updated_at = ?, revision = ? where id = ?`, now(), revision, opts.ClaimID); err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return EditResult{}, err
	}
	return EditResult{EditID: editID, ClaimID: opts.ClaimID, Value: value}, nil
}

func (t *Tideglass) ReviewClaim(ctx context.Context, opts ReviewOptions) (ReviewResult, error) {
	claimID := strings.TrimSpace(opts.ClaimID)
	if claimID == "" {
		return ReviewResult{}, errors.New("claim id is required")
	}
	action := strings.TrimSpace(strings.ToLower(opts.Action))
	status := ""
	switch action {
	case "accept", "accepted":
		action = "accept"
		status = "accepted"
	case "reject", "rejected":
		action = "reject"
		status = "rejected"
	default:
		return ReviewResult{}, fmt.Errorf("unsupported review action %q", opts.Action)
	}
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewResult{}, err
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return ReviewResult{}, err
	}
	res, err := tx.ExecContext(ctx, `update claims set status = ?, updated_at = ?, revision = ? where id = ?`, status, now(), revision, claimID)
	if err != nil {
		_ = tx.Rollback()
		return ReviewResult{}, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return ReviewResult{}, err
	}
	if affected == 0 {
		_ = tx.Rollback()
		return ReviewResult{}, sql.ErrNoRows
	}
	patch, _ := json.Marshal(map[string]string{"status": status})
	if _, err := tx.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
		id("edt"), claimID, action, string(patch), opts.Reason, now()); err != nil {
		_ = tx.Rollback()
		return ReviewResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewResult{}, err
	}
	return ReviewResult{ClaimID: claimID, Action: action, Status: status}, nil
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
	exportEvidence := sanitizeEvidenceForExport(profile.Evidence)
	exportProfile := profile
	exportProfile.Evidence = exportEvidence
	out := opts.Out
	if out == "" {
		out = filepath.Join(filepath.Dir(t.path), "tideglass-"+profile.Intent.Kind+"-"+time.Now().UTC().Format("20060102T150405Z")+".tgz")
	} else {
		out = expandHome(out)
	}
	outPath, err := resolvedWritePath(out)
	if err != nil {
		return ExportResult{}, err
	}
	dbPath, err := resolvedWritePath(t.path)
	if err != nil {
		return ExportResult{}, err
	}
	blockedPaths := map[string]bool{
		dbPath:          true,
		dbPath + "-wal": true,
		dbPath + "-shm": true,
	}
	if blockedPaths[outPath] {
		return ExportResult{}, fmt.Errorf("refusing to export over active database path %s", out)
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
	if err := writeTarJSON(tw, "profile.json", exportProfile); err != nil {
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
	if err := writeTarJSONL(tw, "evidence.jsonl", exportEvidence); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records += len(profile.Evidence)
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

func sanitizeEvidenceForExport(evidence []EvidenceOut) []EvidenceOut {
	out := make([]EvidenceOut, len(evidence))
	for i, ev := range evidence {
		ev.Locator = sanitizeLocatorJSON(ev.Locator)
		out[i] = ev
	}
	return out
}

func sanitizeLocatorJSON(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	value = sanitizeLocatorValue("", value)
	data, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(data)
}

func sanitizeLocatorValue(key string, value any) any {
	switch v := value.(type) {
	case map[string]any:
		for childKey, childValue := range v {
			v[childKey] = sanitizeLocatorValue(childKey, childValue)
		}
		return v
	case []any:
		for i, childValue := range v {
			v[i] = sanitizeLocatorValue(key, childValue)
		}
		return v
	case string:
		lowerKey := strings.ToLower(key)
		if filepath.IsAbs(v) && (strings.Contains(lowerKey, "path") || strings.Contains(lowerKey, "file") || strings.Contains(lowerKey, "database")) {
			return filepath.Base(v)
		}
		return v
	default:
		return value
	}
}

func resolvedWritePath(path string) (string, error) {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent := filepath.Dir(abs)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Clean(filepath.Join(resolvedParent, filepath.Base(abs))), nil
	}
	return filepath.Clean(abs), nil
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

func NewServiceHandler(t *Tideglass) http.Handler {
	return NewServiceHandlerWithToken(t, os.Getenv("TIDEGLASS_SERVICE_TOKEN"))
}

func NewServiceHandlerWithToken(t *Tideglass, token string) http.Handler {
	token = strings.TrimSpace(token)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizedHTTPHost(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		writeHTTPJSON(w, http.StatusOK, map[string]any{"status": "ok", "schema": schemaVersion})
	})
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizedHTTPHost(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if !authorizedServiceRequest(r, token) {
			writeHTTPError(w, http.StatusUnauthorized, "missing or invalid service token")
			return
		}
		if !authorizedHTTPWrite(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden write")
			return
		}
		defer r.Body.Close()
		var request IntentRequestEnvelope
		dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
		dec.UseNumber()
		if err := dec.Decode(&request); err != nil {
			writeHTTPError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := authorizeHTTPResolveRequest(request); err != nil {
			writeHTTPError(w, http.StatusForbidden, err.Error())
			return
		}
		response, err := t.ResolveIntent(r.Context(), ResolveOptions{Request: request})
		if err != nil {
			writeHTTPError(w, statusForResolveError(err), err.Error())
			return
		}
		writeHTTPJSON(w, http.StatusOK, response)
	})
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !authorizedHTTPHost(r) {
			writeHTTPError(w, http.StatusForbidden, "forbidden host")
			return
		}
		if !authorizedServiceRequest(r, token) {
			writeHTTPError(w, http.StatusUnauthorized, "missing or invalid service token")
			return
		}
		uri := r.URL.Query().Get("uri")
		response, err := t.ResolveIntent(r.Context(), ResolveOptions{Request: IntentRequestEnvelope{URI: uri}, NoPersist: true})
		if err != nil {
			writeHTTPError(w, statusForResolveError(err), err.Error())
			return
		}
		writeHTTPJSON(w, http.StatusOK, response)
	})
	return mux
}

func HandleMCPOnce(ctx context.Context, t *Tideglass, in io.Reader, out io.Writer) error {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      any             `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	dec := json.NewDecoder(io.LimitReader(in, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return err
	}
	var result any
	switch req.Method {
	case "resources/read":
		var params struct {
			URI string `json:"uri"`
		}
		if err := decodeJSONUseNumber(req.Params, &params); err != nil {
			return writeMCPError(out, req.ID, -32602, err.Error())
		}
		response, err := t.ResolveIntent(ctx, ResolveOptions{Request: IntentRequestEnvelope{URI: params.URI}, NoPersist: true})
		if err != nil {
			return writeMCPError(out, req.ID, -32000, err.Error())
		}
		text, err := json.MarshalIndent(response, "", "  ")
		if err != nil {
			return writeMCPError(out, req.ID, -32603, err.Error())
		}
		result = map[string]any{"contents": []map[string]any{{"uri": params.URI, "mimeType": "application/json", "text": string(text)}}}
	case "tools/call":
		var params struct {
			Name      string                `json:"name"`
			Arguments IntentRequestEnvelope `json:"arguments"`
		}
		if err := decodeJSONUseNumber(req.Params, &params); err != nil {
			return writeMCPError(out, req.ID, -32602, err.Error())
		}
		if params.Name != "tideglass.resolve_intent" {
			return writeMCPError(out, req.ID, -32601, fmt.Sprintf("unsupported mcp tool %q", params.Name))
		}
		if err := authorizeMCPResolveRequest(params.Arguments); err != nil {
			return writeMCPError(out, req.ID, -32000, err.Error())
		}
		response, err := t.ResolveIntent(ctx, ResolveOptions{Request: params.Arguments})
		if err != nil {
			return writeMCPError(out, req.ID, -32000, err.Error())
		}
		result = map[string]any{"content": []map[string]any{{"type": "text", "text": mustJSONText(response)}}}
	default:
		return writeMCPError(out, req.ID, -32601, fmt.Sprintf("unsupported mcp method %q", req.Method))
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func writeMCPError(out io.Writer, id any, code int, message string) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func decodeJSONUseNumber(data []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(value)
}

func writeHTTPJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	writeHTTPJSON(w, status, map[string]any{"error": message})
}

func authorizedHTTPHost(r *http.Request) bool {
	host := r.Host
	if strings.Contains(host, ":") {
		if parsedHost, _, err := net.SplitHostPort(host); err == nil {
			host = parsedHost
		}
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func authorizedHTTPWrite(r *http.Request) bool {
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType != "application/json" {
		return false
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" && !strings.HasPrefix(origin, "http://127.0.0.1") && !strings.HasPrefix(origin, "http://localhost") && !strings.HasPrefix(origin, "http://[::1]") {
		return false
	}
	return true
}

func authorizedServiceRequest(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Tideglass-Token")), []byte(token)) == 1 {
		return true
	}
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	return strings.HasPrefix(auth, prefix) && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, prefix)), []byte(token)) == 1
}

func authorizeHTTPResolveRequest(request IntentRequestEnvelope) error {
	if request.Disclosure.AllowSensitive {
		return errors.New("http resolve cannot enable sensitive disclosure without a capability token")
	}
	if request.Freshness.RequireReviewed != nil && !*request.Freshness.RequireReviewed {
		return errors.New("http resolve cannot disable reviewed-claim freshness")
	}
	return nil
}

func authorizeMCPResolveRequest(request IntentRequestEnvelope) error {
	if request.Disclosure.AllowSensitive {
		return errors.New("mcp resolve cannot enable sensitive disclosure without trusted server capability")
	}
	if request.Freshness.RequireReviewed != nil && !*request.Freshness.RequireReviewed {
		return errors.New("mcp resolve cannot disable reviewed-claim freshness without trusted server capability")
	}
	return nil
}

func statusForResolveError(err error) int {
	if isRequestError(err) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func isRequestError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "intent request uri is required") ||
		strings.Contains(message, "unsupported intent uri") ||
		strings.Contains(message, "unsupported request schema_version") ||
		strings.Contains(message, "unsupported disclosure mode") ||
		strings.Contains(message, "unsupported task mode") ||
		strings.Contains(message, "unsupported autonomy") ||
		strings.Contains(message, "invalid freshness.max_age")
}

func mustJSONText(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(data)
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

func (t *Tideglass) findIntentForResolve(ctx context.Context, kind string) (IntentOut, bool, error) {
	var intent IntentOut
	err := t.db.QueryRowContext(ctx, `select id,kind,title from intents where kind = ? and status = 'active' order by updated_at desc limit 1`, kind).Scan(&intent.ID, &intent.Kind, &intent.Title)
	if err == nil {
		return intent, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return IntentOut{}, false, nil
	}
	return IntentOut{}, false, err
}

func (t *Tideglass) persistIntentRequest(ctx context.Context, request IntentRequestEnvelope, requestID string) (string, error) {
	actorJSON, err := json.Marshal(request.Actor)
	if err != nil {
		return "", err
	}
	freshnessJSON, err := json.Marshal(request.Freshness)
	if err != nil {
		return "", err
	}
	disclosureJSON, err := json.Marshal(request.Disclosure)
	if err != nil {
		return "", err
	}
	contextJSON, err := json.Marshal(nonNilMap(request.Context))
	if err != nil {
		return "", err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	_, err = t.db.ExecContext(ctx, `
insert into intent_requests(id,uri,actor_json,purpose,audience,freshness_json,disclosure_json,context_json,request_json,created_at)
values(?,?,?,?,?,?,?,?,?,?)`,
		requestID, request.URI, string(actorJSON), legacyPurpose(request), request.Audience.Label(), string(freshnessJSON), string(disclosureJSON), string(contextJSON), string(requestJSON), now())
	return requestID, err
}

func (t *Tideglass) persistProfileSnapshot(ctx context.Context, requestID string, response IntentResponseEnvelope) (string, error) {
	snapshotID := id("snap")
	response.SnapshotID = snapshotID
	if hasCommitments(response.Commitments) {
		response.Commitments.SnapshotID = snapshotID
	}
	responseJSON, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	intentID := response.Intent.ID
	_, err = t.db.ExecContext(ctx, `
insert into profile_snapshots(id,request_id,uri,resolved_uri,intent_id,response_json,profile_hash,created_at)
values(?,?,?,?,?,?,?,?)`,
		snapshotID, requestID, response.URI, response.ResolvedURI, intentID, string(responseJSON), response.ProfileHash, now())
	return snapshotID, err
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

func (t *Tideglass) mergePersistedSource(ctx context.Context, source SourceStatus) (SourceStatus, error) {
	var persisted SourceStatus
	var capsJSON, countsJSON, metadataJSON string
	err := t.db.QueryRowContext(ctx, `
select id,kind,label,locator,health,capabilities_json,counts_json,coalesce(last_probe_at,''),last_error,metadata_json
from sources where id = ?`, source.ID).Scan(
		&persisted.ID, &persisted.Kind, &persisted.Label, &persisted.Locator, &persisted.Health,
		&capsJSON, &countsJSON, &persisted.LastProbeAt, &persisted.LastError, &metadataJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return source, nil
	}
	if err != nil {
		return SourceStatus{}, err
	}
	_ = json.Unmarshal([]byte(capsJSON), &persisted.Capabilities)
	_ = json.Unmarshal([]byte(countsJSON), &persisted.Counts)
	_ = json.Unmarshal([]byte(metadataJSON), &persisted.Metadata)
	if strings.TrimSpace(source.Kind) == "" {
		source.Kind = persisted.Kind
	}
	if strings.TrimSpace(source.Label) == "" {
		source.Label = persisted.Label
	}
	if persisted.Kind == "import" && strings.TrimSpace(persisted.Locator) != "" {
		source.Locator = persisted.Locator
	} else if strings.TrimSpace(source.Locator) == "" {
		source.Locator = persisted.Locator
	}
	if source.Health == "" || source.Health == "unknown" {
		source.Health = persisted.Health
	}
	if len(source.Capabilities) == 0 {
		source.Capabilities = persisted.Capabilities
	}
	if len(source.Counts) == 0 {
		source.Counts = persisted.Counts
	}
	if strings.TrimSpace(source.LastProbeAt) == "" {
		source.LastProbeAt = persisted.LastProbeAt
	}
	if strings.TrimSpace(source.LastError) == "" {
		source.LastError = persisted.LastError
	}
	source.Metadata = mergeStringMaps(persisted.Metadata, source.Metadata)
	return source, nil
}

func (t *Tideglass) claimExistsForEvidence(ctx context.Context, evidenceID, kind string) (bool, error) {
	var existing string
	err := t.db.QueryRowContext(ctx, `
select c.id
from claims c
join claim_evidence ce on ce.claim_id = c.id
where ce.evidence_id = ? and c.kind = ?
limit 1`, evidenceID, kind).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return "", err
	}
	claimID := id("clm")
	_, err = tx.ExecContext(ctx, `
insert into claims(id,intent_id,subject,kind,value,normalized_value,polarity,scope,status,source_mode,confidence,created_at,updated_at,revision)
values(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		claimID, intentID, "user", claim.Kind, claim.Value, normalizeText(claim.Value), "neutral", "", "active", sourceMode, claim.Confidence, now(), now(), revision)
	if err != nil {
		_ = tx.Rollback()
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return claimID, nil
}

type revisionExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func nextRevision(ctx context.Context, execer revisionExecer) (int64, error) {
	result, err := execer.ExecContext(ctx, `insert into revisions default values`)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (t *Tideglass) loadClaims(ctx context.Context, intentID string) ([]ClaimOut, error) {
	rows, err := t.db.QueryContext(ctx, `
select c.id, c.kind,
       coalesce(json_extract(e.patch_json, '$.value'), c.value) as value,
       c.confidence, c.status, c.source_mode,
       c.created_at, c.updated_at, coalesce(e.created_at, ''),
       case when e.id is null then 0 else 1 end as has_value_edit
from claims c
left join edits e on e.id = (
  select id from edits
  where claim_id = c.id and json_extract(patch_json, '$.value') is not null
  order by julianday(created_at) desc, rowid desc limit 1
)
where c.intent_id = ?
order by c.created_at desc`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type materializedClaim struct {
		ClaimOut
		CreatedAt    string
		UpdatedAt    string
		HasValueEdit bool
		Normalized   string
	}
	var rowsOut []materializedClaim
	for rows.Next() {
		var claim materializedClaim
		var hasValueEdit int
		var valueEditCreatedAt string
		if err := rows.Scan(&claim.ID, &claim.Kind, &claim.Value, &claim.Confidence, &claim.Status, &claim.SourceMode, &claim.CreatedAt, &claim.UpdatedAt, &valueEditCreatedAt, &hasValueEdit); err != nil {
			return nil, err
		}
		if timestampAfter(valueEditCreatedAt, claim.UpdatedAt) {
			claim.UpdatedAt = valueEditCreatedAt
		}
		claim.HasValueEdit = hasValueEdit == 1
		claim.ClaimOut.UpdatedAt = claim.UpdatedAt
		claim.Normalized = normalizeText(claim.Value)
		rowsOut = append(rowsOut, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	decisionRank := func(status string) int {
		switch status {
		case "accepted":
			return 3
		case "rejected":
			return 2
		default:
			return 1
		}
	}
	prefer := func(left, right materializedClaim, rank func(string) int) bool {
		if rank(left.Status) != rank(right.Status) {
			return rank(left.Status) > rank(right.Status)
		}
		if left.UpdatedAt != right.UpdatedAt {
			return timestampAfter(left.UpdatedAt, right.UpdatedAt)
		}
		if left.HasValueEdit != right.HasValueEdit {
			return left.HasValueEdit
		}
		if left.Confidence != right.Confidence {
			return left.Confidence > right.Confidence
		}
		return left.CreatedAt > right.CreatedAt
	}
	sort.SliceStable(rowsOut, func(leftIndex, rightIndex int) bool {
		return prefer(rowsOut[leftIndex], rowsOut[rightIndex], decisionRank)
	})
	duplicateSeen := map[string]bool{}
	var deduped []materializedClaim
	for _, claim := range rowsOut {
		key := claim.Kind + "\x00" + claim.Normalized
		if duplicateSeen[key] {
			continue
		}
		duplicateSeen[key] = true
		deduped = append(deduped, claim)
	}
	slotRank := func(status string) int {
		switch status {
		case "accepted":
			return 3
		case "active":
			return 2
		default:
			return 1
		}
	}
	sort.SliceStable(deduped, func(leftIndex, rightIndex int) bool {
		return prefer(deduped[leftIndex], deduped[rightIndex], slotRank)
	})
	seen := map[string]bool{}
	var claims []ClaimOut
	for _, claim := range deduped {
		key := claim.Kind + "\x00" + claim.Normalized
		if singletonClaimKind(claim.Kind) {
			key = claim.Kind
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if claim.Status == "rejected" {
			continue
		}
		claims = append(claims, claim.ClaimOut)
	}
	return claims, nil
}

func (t *Tideglass) loadActionGateClaims(ctx context.Context, intentID, intentKind string, requiredSlots []string) ([]ClaimOut, error) {
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range requiredSlots {
		if strings.TrimSpace(slot) != "" {
			blocking[strings.TrimSpace(slot)] = true
		}
	}
	rows, err := t.db.QueryContext(ctx, `
select c.id, c.kind,
       coalesce(json_extract(e.patch_json, '$.value'), c.value) as value,
       c.confidence, c.status, c.source_mode,
       c.updated_at, coalesce(e.created_at, ''), c.revision
from claims c
left join edits e on e.id = (
  select id from edits
  where claim_id = c.id and json_extract(patch_json, '$.value') is not null
  order by julianday(created_at) desc, rowid desc limit 1
)
where c.intent_id = ? and c.status != 'rejected'
order by c.created_at desc`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ClaimOut
	bestAcceptedSingleton := map[string]int64{}
	actionGateClaimRevisions := map[string]int64{}
	for rows.Next() {
		var claim ClaimOut
		var valueEditCreatedAt string
		var revision int64
		if err := rows.Scan(&claim.ID, &claim.Kind, &claim.Value, &claim.Confidence, &claim.Status, &claim.SourceMode, &claim.UpdatedAt, &valueEditCreatedAt, &revision); err != nil {
			return nil, err
		}
		if !blocking[claim.Kind] && !strings.HasPrefix(claim.Kind, "boundary.") {
			continue
		}
		if timestampAfter(valueEditCreatedAt, claim.UpdatedAt) {
			claim.UpdatedAt = valueEditCreatedAt
		}
		if singletonClaimKind(claim.Kind) && claim.Status == "accepted" && revision > bestAcceptedSingleton[claim.Kind] {
			bestAcceptedSingleton[claim.Kind] = revision
		}
		claims = append(claims, claim)
		actionGateClaimRevisions[claim.ID] = revision
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ClaimOut, 0, len(claims))
	for _, claim := range claims {
		revision := actionGateClaimRevisions[claim.ID]
		if singletonClaimKind(claim.Kind) && bestAcceptedSingleton[claim.Kind] > 0 && revision > 0 && revision < bestAcceptedSingleton[claim.Kind] {
			continue
		}
		out = append(out, claim)
	}
	return out, nil
}

func (t *Tideglass) attachClaimEvidence(ctx context.Context, claims []ClaimOut) ([]ClaimOut, []EvidenceOut, error) {
	if len(claims) == 0 {
		return claims, nil, nil
	}
	evidenceByID := map[string]EvidenceOut{}
	var evidence []EvidenceOut
	for index := range claims {
		rows, err := t.db.QueryContext(ctx, `
select e.id, sa.source_id, e.locator_json, e.snippet
from claim_evidence ce
join evidence e on e.id = ce.evidence_id
join source_artifacts sa on sa.id = e.source_artifact_id
where ce.claim_id = ?
order by e.observed_at desc, e.id`, claims[index].ID)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var ev EvidenceOut
			if err := rows.Scan(&ev.ID, &ev.SourceID, &ev.Locator, &ev.Snippet); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			claims[index].Evidence = append(claims[index].Evidence, ev.ID)
			if _, ok := evidenceByID[ev.ID]; !ok {
				evidenceByID[ev.ID] = ev
				evidence = append(evidence, ev)
			}
		}
		if err := rows.Close(); err != nil {
			return nil, nil, err
		}
	}
	return claims, evidence, nil
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

func sqliteColumnExists(ctx context.Context, db *sql.DB, table, column string) bool {
	rows, err := db.QueryContext(ctx, `pragma table_info(`+store.QuoteIdent(table)+`)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var primaryKey int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return false
		}
		if strings.EqualFold(name, column) {
			return true
		}
	}
	return false
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

func (t *Tideglass) ensureIntentRequestSchema(ctx context.Context) error {
	if !sqliteColumnExists(ctx, t.db, "intent_requests", "request_json") {
		if _, err := t.db.ExecContext(ctx, `alter table intent_requests add column request_json text not null default '{}'`); err != nil {
			return err
		}
	}
	if _, err := t.db.ExecContext(ctx, `create table if not exists revisions (id integer primary key autoincrement)`); err != nil {
		return err
	}
	if !sqliteColumnExists(ctx, t.db, "claims", "revision") {
		if _, err := t.db.ExecContext(ctx, `alter table claims add column revision integer not null default 0`); err != nil {
			return err
		}
	}
	return nil
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
	bin, err := crawlbarBinary()
	if err != nil {
		return nil, err
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(discoveryCtx, bin, "apps", "--json")
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

func crawlbarBinary() (string, error) {
	for _, envKey := range []string{"TIDEGLASS_CRAWLBAR", "CRAWLBAR_BIN"} {
		if value := strings.TrimSpace(os.Getenv(envKey)); value != "" {
			return value, nil
		}
	}
	if bin, err := exec.LookPath("crawlbar"); err == nil {
		return bin, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		bin := filepath.Join(home, ".local", "bin", "crawlbar")
		if _, statErr := os.Stat(bin); statErr == nil {
			return bin, nil
		}
	}
	return "", errors.New("crawlbar binary not found")
}

func crawlbarDatabasePath(ctx context.Context, bin, appID string) string {
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(statusCtx, bin, "status", "--app", appID, "--json")
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
	source.LastError = ""
	switch source.ID {
	case "gitcrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"threads": "threads", "clusters": "cluster_groups"}, []string{"metadata", "text"})
	case "slacrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"messages": "messages", "channels": "channels", "users": "users", "search": "message_fts"}, []string{"fts", "metadata", "text"})
	case "discrawl":
		out := probeSQLite(ctx, source, source.Locator, map[string]string{"messages": "messages", "members": "members", "embeddings": "message_embeddings", "search": "message_fts"}, []string{"fts", "semantic", "metadata", "text"})
		if out.Health == "ok" && out.Counts["embeddings"] == 0 {
			out.Health = "partial"
			out.LastError = "discrawl has no message embeddings; direct text probe is usable"
		}
		return out
	case "notcrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"pages": "pages", "blocks": "blocks", "comments": "comments", "search": "page_fts"}, []string{"fts", "metadata", "text"})
	case "graincrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"notes": "notes", "transcripts": "transcript_chunks", "search": "notes_fts"}, []string{"fts", "metadata", "text"})
	case "codex":
		count := int64(0)
		err := filepath.WalkDir(expandHome(source.Locator), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
				count++
			}
			return nil
		})
		source.Capabilities = []string{"metadata", "text"}
		source.Counts = map[string]int64{"session_files": count}
		if err != nil {
			source.Health = "error"
			source.LastError = err.Error()
			return source
		}
		source.Health = "ok"
		return source
	default:
		if source.Kind == "import" {
			if source.Health == "" {
				source.Health = "unknown"
			}
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
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
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
			return nil, nil, err
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
	var artifacts []artifact
	if info.IsDir() {
		artifacts, err = importAssistantDir(kind, path, limit)
	} else if strings.HasSuffix(strings.ToLower(path), ".zip") {
		artifacts, err = importAssistantZip(kind, path, limit)
	} else {
		data, readErr := readFileLimited(path)
		err = readErr
		if err == nil {
			artifacts, err = appendAssistantArtifacts(kind, nil, namedBlob{Name: filepath.Base(path), Data: data}, limit)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	var claims []candidateClaim
	return artifacts, claims, nil
}

type namedBlob struct {
	Name string
	Data []byte
}

func importAssistantZip(kind, path string, limit int) ([]artifact, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var artifacts []artifact
	for _, file := range zr.File {
		if limit > 0 && len(artifacts) >= limit {
			break
		}
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".json") {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, err := readAllLimited(rc, assistantJSONLimit, file.Name)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		artifacts, err = appendAssistantArtifacts(kind, artifacts, namedBlob{Name: file.Name, Data: data}, limit)
		if err != nil {
			return nil, err
		}
	}
	return artifacts, nil
}

func importAssistantDir(kind, root string, limit int) ([]artifact, error) {
	var artifacts []artifact
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if limit > 0 && len(artifacts) >= limit {
			return filepath.SkipAll
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}
		data, err := readFileLimited(path)
		if err != nil {
			return err
		}
		artifacts, err = appendAssistantArtifacts(kind, artifacts, namedBlob{Name: path, Data: data}, limit)
		return err
	})
	return artifacts, err
}

func appendAssistantArtifacts(kind string, artifacts []artifact, blob namedBlob, limit int) ([]artifact, error) {
	var value any
	if err := json.Unmarshal(blob.Data, &value); err != nil {
		return artifacts, fmt.Errorf("parse %s: %w", blob.Name, err)
	}
	texts := collectAssistantTexts(value)
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
	return artifacts, nil
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

func unresolvedIntentQuestions(kind string, claims []ClaimOut) []IntentQuestion {
	have := map[string]bool{}
	for _, claim := range claims {
		have[claim.Kind] = true
	}
	need := map[string][]struct {
		Kind     string
		Question string
		Priority string
	}{
		"social.dinner": {
			{"preference.food.allergy", "Any allergies or hard dietary restrictions?", "critical"},
			{"preference.food.budget", "What dinner budget range is comfortable?", "normal"},
			{"preference.social.group_size", "What group size feels good with strangers?", "normal"},
			{"boundary.social.topic", "Any topics to avoid with strangers?", "normal"},
		},
		"social.dating": {
			{"preference.dating.relationship_goal", "What relationship goal should be explicit?", "critical"},
			{"boundary.dating.dealbreaker", "What dealbreakers should never be inferred?", "critical"},
			{"boundary.dating.safety", "What safety boundaries should be explicit?", "critical"},
		},
		"work.new_job": {
			{"preference.work.communication", "What communication cadence should a new team know?", "normal"},
			{"boundary.work.focus_time", "What focus-time boundaries matter?", "normal"},
		},
		"work.project.start": {
			{"boundary.project.no_go", "Any project-specific no-go areas?", "normal"},
			{"context.project.related_repos", "Which repos are definitely in scope?", "normal"},
		},
	}
	rows := need[kind]
	if len(rows) == 0 && strings.HasPrefix(kind, "work.") {
		rows = need["work.project.start"]
	}
	out := make([]IntentQuestion, 0, len(rows))
	for _, row := range rows {
		if !have[row.Kind] {
			question := IntentQuestion{
				Kind:     row.Kind,
				Slot:     row.Kind,
				Question: row.Question,
				Priority: row.Priority,
			}
			question.BlocksAction = question.Priority == "critical"
			question.AnswerType = "short_text"
			out = append(out, question)
		}
	}
	return out
}

func legacyPurpose(request IntentRequestEnvelope) string {
	if strings.TrimSpace(request.Purpose) != "" {
		return strings.TrimSpace(request.Purpose)
	}
	return strings.TrimSpace(request.Task.Goal)
}

func addRequiredSlotQuestions(unresolved []IntentQuestion, claims []ClaimOut, requiredSlots []string) []IntentQuestion {
	if len(requiredSlots) == 0 {
		return unresolved
	}
	haveClaims := map[string]bool{}
	for _, claim := range claims {
		haveClaims[claim.Kind] = true
	}
	out := append([]IntentQuestion{}, unresolved...)
	for _, slot := range requiredSlots {
		slot = strings.TrimSpace(slot)
		if slot == "" || haveClaims[slot] {
			continue
		}
		promoted := false
		for index := range out {
			if questionSlot(out[index]) == slot {
				out[index].Priority = "critical"
				out[index].BlocksAction = true
				promoted = true
			}
		}
		if promoted {
			continue
		}
		out = append(out, questionForSlot(slot, "critical", true))
	}
	return out
}

func redactedBlockingQuestions(intentKind string, redacted []string, requiredSlots []string, existing []IntentQuestion) []IntentQuestion {
	if len(redacted) == 0 {
		return nil
	}
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range requiredSlots {
		if strings.TrimSpace(slot) != "" {
			blocking[strings.TrimSpace(slot)] = true
		}
	}
	haveBlockingQuestions := map[string]bool{}
	for _, question := range existing {
		haveBlockingQuestions[questionSlot(question)] = question.Priority == "critical" || question.BlocksAction
	}
	var out []IntentQuestion
	for _, kind := range redacted {
		if (blocking[kind] || strings.HasPrefix(kind, "boundary.")) && !haveBlockingQuestions[kind] {
			out = append(out, questionForSlot(kind, "critical", true))
		}
	}
	return out
}

func policyFailedBlockingQuestions(intentKind string, claims []ClaimOut, requiredSlots []string, existing []IntentQuestion) []IntentQuestion {
	if len(claims) == 0 {
		return nil
	}
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range requiredSlots {
		if strings.TrimSpace(slot) != "" {
			blocking[strings.TrimSpace(slot)] = true
		}
	}
	haveBlockingQuestions := map[string]bool{}
	for _, question := range existing {
		haveBlockingQuestions[questionSlot(question)] = question.Priority == "critical" || question.BlocksAction
	}
	var out []IntentQuestion
	for _, claim := range claims {
		if (blocking[claim.Kind] || strings.HasPrefix(claim.Kind, "boundary.")) && !haveBlockingQuestions[claim.Kind] {
			out = append(out, questionForSlot(claim.Kind, "critical", true))
		}
	}
	return out
}

func questionForSlot(slot, priority string, blocksAction bool) IntentQuestion {
	return IntentQuestion{
		Kind:         slot,
		Slot:         slot,
		Question:     defaultSlotQuestion(slot),
		Priority:     priority,
		BlocksAction: blocksAction,
		AnswerType:   "short_text",
	}
}

func defaultSlotQuestion(slot string) string {
	switch slot {
	case "preference.food.allergy":
		return "Any allergies or hard dietary restrictions?"
	case "preference.food.dietary_restriction":
		return "Any dietary restrictions?"
	case "preference.budget.restaurant", "preference.food.budget":
		return "What budget range is comfortable?"
	default:
		return "What should Tideglass know for " + slot + "?"
	}
}

func questionSlot(question IntentQuestion) string {
	if question.Slot != "" {
		return question.Slot
	}
	return question.Kind
}

func normalizeIntentRequest(request IntentRequestEnvelope) IntentRequestEnvelope {
	request.SchemaVersion = strings.TrimSpace(request.SchemaVersion)
	if request.SchemaVersion == "" {
		request.SchemaVersion = "tideglass.intent_request.v2"
	}
	request.URI = strings.TrimSpace(request.URI)
	request.Actor.Type = strings.TrimSpace(request.Actor.Type)
	if request.Actor.Type == "" {
		request.Actor.Type = "agent"
	}
	request.Actor.ID = strings.TrimSpace(request.Actor.ID)
	if request.Actor.ID == "" {
		request.Actor.ID = "local"
	}
	request.Actor.TrustTier = strings.TrimSpace(request.Actor.TrustTier)
	if request.Actor.TrustTier == "" {
		request.Actor.TrustTier = "local"
	}
	request.Task.Mode = strings.ToLower(strings.TrimSpace(request.Task.Mode))
	if request.Task.Mode == "" {
		request.Task.Mode = "context"
	}
	request.Task.Autonomy = strings.ToLower(strings.TrimSpace(request.Task.Autonomy))
	if request.Task.Autonomy == "" {
		request.Task.Autonomy = "suggest_only"
	}
	if request.Task.Goal == "" && strings.TrimSpace(request.Purpose) != "" {
		request.Task.Goal = strings.TrimSpace(request.Purpose)
	}
	request.Audience.Type = strings.TrimSpace(request.Audience.Type)
	request.Audience.ID = strings.TrimSpace(request.Audience.ID)
	if request.Audience.Type == "" && request.Audience.ID == "" {
		request.Audience.Type = "agent"
	}
	request.Disclosure.Mode = strings.ToLower(strings.TrimSpace(request.Disclosure.Mode))
	if request.Disclosure.Mode == "" {
		request.Disclosure.Mode = "minimal"
	}
	if request.Disclosure.AllowValues == nil {
		defaultAllowValues := request.Disclosure.Mode != "existence"
		request.Disclosure.AllowValues = &defaultAllowValues
	}
	if request.Disclosure.AllowCommitments == nil {
		defaultAllowCommitments := true
		request.Disclosure.AllowCommitments = &defaultAllowCommitments
	}
	request.Contract.Output = strings.TrimSpace(request.Contract.Output)
	if request.Contract.Output == "" {
		request.Contract.Output = "decision_contract"
	}
	request.Proof.Hash = strings.TrimSpace(request.Proof.Hash)
	if request.Proof.Hash == "" {
		request.Proof.Hash = "response"
	}
	request.Proof.Commitments = strings.TrimSpace(request.Proof.Commitments)
	if request.Proof.Commitments == "" {
		request.Proof.Commitments = "per_claim"
	}
	if request.Context == nil {
		request.Context = map[string]any{}
	}
	return request
}

func validateIntentRequest(request IntentRequestEnvelope) error {
	switch request.SchemaVersion {
	case "tideglass.intent_request.v2", "tideglass.intent_request.v1", "":
	default:
		return fmt.Errorf("unsupported request schema_version %q", request.SchemaVersion)
	}
	switch request.Disclosure.Mode {
	case "full", "minimal", "existence":
	default:
		return fmt.Errorf("unsupported disclosure mode %q", request.Disclosure.Mode)
	}
	switch request.Task.Mode {
	case "context", "slot_fill", "act_gate", "compare", "handoff":
	default:
		return fmt.Errorf("unsupported task mode %q", request.Task.Mode)
	}
	switch request.Task.Autonomy {
	case "context_only", "suggest_only", "suggest_then_confirm", "bounded_act", "deny":
	default:
		return fmt.Errorf("unsupported autonomy %q", request.Task.Autonomy)
	}
	return nil
}

func parseIntentURI(rawURI string) (string, string, error) {
	uri := strings.TrimSpace(rawURI)
	if uri == "" {
		return "", "", errors.New("intent request uri is required")
	}
	if !strings.HasPrefix(uri, "tideglass://") {
		return "", "", fmt.Errorf("unsupported intent uri %q", rawURI)
	}
	rest := strings.TrimPrefix(uri, "tideglass://")
	parts := strings.Split(rest, "/")
	if len(parts) > 0 && parts[0] == "v1" {
		if len(parts) > 1 && parts[1] == "v1" {
			return "", "", fmt.Errorf("unsupported intent uri %q", rawURI)
		}
		parts = parts[1:]
	}
	switch {
	case len(parts) == 2 && parts[0] == "intent" && strings.TrimSpace(parts[1]) != "":
		return normalizeKind(parts[1], ""), "", nil
	case len(parts) == 3 && parts[0] == "intent" && strings.TrimSpace(parts[1]) != "" && parts[2] == "current":
		return normalizeKind(parts[1], ""), "", nil
	case len(parts) == 4 && parts[0] == "profile" && parts[1] == "me" && strings.TrimSpace(parts[2]) != "" && parts[3] == "current":
		return normalizeKind(parts[2], ""), "", nil
	case len(parts) == 2 && parts[0] == "unresolved" && strings.TrimSpace(parts[1]) != "":
		return normalizeKind(parts[1], ""), "", nil
	case len(parts) == 3 && parts[0] == "disclosure" && strings.TrimSpace(parts[1]) != "" && strings.TrimSpace(parts[2]) != "":
		return normalizeKind(parts[1], ""), parts[2], nil
	default:
		return "", "", fmt.Errorf("unsupported intent uri %q", rawURI)
	}
}

func eligibleClaimsForRequest(claims []ClaimOut, requireReviewed bool, maxAge time.Duration) []ClaimOut {
	out := make([]ClaimOut, 0, len(claims))
	for _, claim := range claims {
		if requireReviewed && claim.Status != "accepted" {
			continue
		}
		if maxAge > 0 && claim.UpdatedAt != "" {
			updatedAt, err := time.Parse(time.RFC3339, claim.UpdatedAt)
			if err != nil || time.Since(updatedAt) > maxAge {
				continue
			}
		}
		out = append(out, claim)
	}
	return out
}

func policyFailingClaims(loaded []ClaimOut, eligible []ClaimOut) []ClaimOut {
	eligibleByID := map[string]bool{}
	for _, claim := range eligible {
		eligibleByID[claim.ID] = true
	}
	var out []ClaimOut
	for _, claim := range loaded {
		if !eligibleByID[claim.ID] {
			out = append(out, claim)
		}
	}
	return out
}

func actionGatePolicyFailingClaims(intentKind string, claims []ClaimOut, request IntentRequestEnvelope, maxAge time.Duration) []ClaimOut {
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range request.Contract.RequiredSlots {
		if strings.TrimSpace(slot) != "" {
			blocking[strings.TrimSpace(slot)] = true
		}
	}
	var out []ClaimOut
	for _, claim := range claims {
		if !blocking[claim.Kind] && !strings.HasPrefix(claim.Kind, "boundary.") {
			continue
		}
		if claim.Status != "accepted" || claimStale(claim, maxAge) || belowConfidenceFloor(claim, request) {
			out = append(out, claim)
		}
	}
	return out
}

func hasActionGateConstraint(intentKind string, claims []ClaimOut, request IntentRequestEnvelope, maxAge time.Duration) bool {
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range request.Contract.RequiredSlots {
		if strings.TrimSpace(slot) != "" {
			blocking[strings.TrimSpace(slot)] = true
		}
	}
	for _, claim := range claims {
		if claim.Status != "accepted" || claimStale(claim, maxAge) || belowConfidenceFloor(claim, request) {
			continue
		}
		if blocking[claim.Kind] || strings.HasPrefix(claim.Kind, "boundary.") {
			return true
		}
	}
	return false
}

func mergeClaimOuts(primary []ClaimOut, extra []ClaimOut) []ClaimOut {
	seen := map[string]bool{}
	out := make([]ClaimOut, 0, len(primary)+len(extra))
	for _, claim := range primary {
		seen[claim.ID] = true
		out = append(out, claim)
	}
	for _, claim := range extra {
		if seen[claim.ID] {
			continue
		}
		seen[claim.ID] = true
		out = append(out, claim)
	}
	return out
}

func claimsForSlotSatisfaction(intentKind string, claims []ClaimOut, request IntentRequestEnvelope, maxAge time.Duration) []ClaimOut {
	critical := criticalClaimKinds(intentKind)
	required := map[string]bool{}
	for _, slot := range request.Contract.RequiredSlots {
		if strings.TrimSpace(slot) != "" {
			required[strings.TrimSpace(slot)] = true
		}
	}
	out := make([]ClaimOut, 0, len(claims))
	for _, claim := range claims {
		if claimStale(claim, maxAge) {
			continue
		}
		if request.Task.Mode == "act_gate" && claim.Status != "accepted" {
			continue
		}
		if claim.Status != "accepted" {
			if !request.Freshness.AcceptInferredForQuestions {
				continue
			}
			if critical[claim.Kind] || required[claim.Kind] {
				continue
			}
		}
		if belowConfidenceFloor(claim, request) {
			continue
		}
		out = append(out, claim)
	}
	return out
}

func claimStale(claim ClaimOut, maxAge time.Duration) bool {
	if maxAge <= 0 || claim.UpdatedAt == "" {
		return false
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, claim.UpdatedAt)
	return err != nil || time.Since(updatedAt) > maxAge
}

func timestampAfter(left, right string) bool {
	if left == "" || left == right {
		return false
	}
	if right == "" {
		return true
	}
	leftTime, leftErr := time.Parse(time.RFC3339Nano, left)
	rightTime, rightErr := time.Parse(time.RFC3339Nano, right)
	if leftErr == nil && rightErr == nil {
		return leftTime.After(rightTime)
	}
	return left > right
}

func belowConfidenceFloor(claim ClaimOut, request IntentRequestEnvelope) bool {
	return request.Contract.ConfidenceFloor > 0 && claim.Confidence < request.Contract.ConfidenceFloor
}

func parseMaxAge(value string) (time.Duration, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return 0, nil
	}
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid freshness.max_age %q", value)
		}
		if int64(days) > math.MaxInt64/int64(24*time.Hour) {
			return 0, fmt.Errorf("invalid freshness.max_age %q", value)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid freshness.max_age %q", value)
	}
	return duration, nil
}

func applyIntentPolicy(intentKind string, claims []ClaimOut, unresolved []IntentQuestion, request IntentRequestEnvelope, allowAction bool) ([]IntentClaimEnvelope, IntentPolicyEnvelope) {
	freshness := "reviewed"
	if request.Freshness.RequireReviewed != nil && !*request.Freshness.RequireReviewed {
		freshness = "unreviewed_allowed"
	}
	if request.Freshness.MaxAge != "" {
		freshness += "_" + request.Freshness.MaxAge
	}
	policy := IntentPolicyEnvelope{
		Audience:        request.Audience.Label(),
		DisclosureMode:  request.Disclosure.Mode,
		SafeToShare:     []string{},
		Redacted:        []string{},
		NeedsUserAnswer: hasCriticalUnresolved(unresolved),
		Freshness:       freshness,
	}
	out := make([]IntentClaimEnvelope, 0, len(claims))
	redacted := map[string]bool{}
	shareable := map[string]bool{}
	for _, claim := range claims {
		if request.Contract.ConfidenceFloor > 0 && claim.Confidence < request.Contract.ConfidenceFloor {
			continue
		}
		if request.Disclosure.Mode == "minimal" && !claimRelevantToRequest(claim.Kind, request) && !claimRequiredForActionGate(intentKind, claim.Kind, request) {
			continue
		}
		sensitivity := claimSensitivity(claim.Kind)
		envelope := IntentClaimEnvelope{
			ID:          claim.ID,
			Kind:        claim.Kind,
			Confidence:  claim.Confidence,
			Status:      claim.Status,
			SourceMode:  claim.SourceMode,
			Sensitivity: sensitivity,
			FreshAt:     claim.UpdatedAt,
		}
		if allowClaimValues(request.Disclosure) {
			envelope.Value = claim.Value
		}
		if request.Disclosure.AllowEvidence {
			envelope.Evidence = claim.Evidence
		}
		if shouldRedactClaim(request.Disclosure, sensitivity) {
			redacted[claim.Kind] = true
			if request.Disclosure.Mode == "existence" {
				out = append(out, existenceClaimEnvelope(claim))
			}
			continue
		}
		if request.Disclosure.Mode == "existence" {
			out = append(out, existenceClaimEnvelope(claim))
			shareable[claim.Kind] = true
			continue
		}
		shareable[claim.Kind] = true
		out = append(out, envelope)
	}
	for kind := range shareable {
		policy.SafeToShare = append(policy.SafeToShare, kind)
	}
	for kind := range redacted {
		policy.Redacted = append(policy.Redacted, kind)
	}
	sort.Strings(policy.SafeToShare)
	sort.Strings(policy.Redacted)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	policy.MayAct = !policy.NeedsUserAnswer
	if request.Task.Mode == "act_gate" && (!allowClaimValues(request.Disclosure) || request.Disclosure.Mode == "existence") {
		policy.NeedsUserAnswer = true
		policy.MayAct = false
	}
	if !allowAction || request.Task.Mode != "act_gate" || request.Task.Autonomy == "context_only" || request.Task.Autonomy == "suggest_only" || request.Task.Autonomy == "suggest_then_confirm" || request.Task.Autonomy == "deny" {
		policy.MayAct = false
	}
	return out, policy
}

func claimRelevantToRequest(kind string, request IntentRequestEnvelope) bool {
	for _, slot := range request.Contract.RequiredSlots {
		if kind == strings.TrimSpace(slot) {
			return true
		}
	}
	for _, slot := range request.Contract.OptionalSlots {
		if kind == strings.TrimSpace(slot) {
			return true
		}
	}
	if len(request.Contract.RequiredSlots) > 0 || len(request.Contract.OptionalSlots) > 0 {
		return false
	}
	audience := request.Audience.Label()
	return audience == "agent" || audience == "local" || audience == request.Actor.ID
}

func claimRequiredForActionGate(intentKind, kind string, request IntentRequestEnvelope) bool {
	return request.Task.Mode == "act_gate" && (strings.HasPrefix(kind, "boundary.") || criticalClaimKinds(intentKind)[kind])
}

func addClaimCommitments(claims []IntentClaimEnvelope, request IntentRequestEnvelope, commitments map[string]string) []IntentClaimEnvelope {
	if !allowCommitments(request.Disclosure) {
		return claims
	}
	out := make([]IntentClaimEnvelope, len(claims))
	for index, claim := range claims {
		out[index] = claim
		if claim.ID != "" {
			out[index].Commitment = commitments[claim.ID]
		}
		if out[index].Commitment == "" {
			out[index].Commitment = commitments[claim.Kind]
		}
	}
	return out
}

func claimCommitmentsForClaims(claims []ClaimOut) map[string]string {
	out := map[string]string{}
	for _, claim := range claims {
		commitment := claimCommitment(claim)
		out[claim.ID] = commitment
		if _, exists := out[claim.Kind]; !exists {
			out[claim.Kind] = commitment
		}
	}
	return out
}

func claimCommitment(claim ClaimOut) string {
	data, _ := json.Marshal(map[string]string{
		"kind":   claim.Kind,
		"status": claim.Status,
		"value":  claim.Value,
	})
	return "sha256:" + hashBytes(data)
}

func hasCommitments(commitments IntentCommitments) bool {
	return commitments.ResponseHash != "" || commitments.ClaimRoot != "" || commitments.SnapshotID != "" || commitments.Algorithm != ""
}

func claimRoot(claims []IntentClaimEnvelope) string {
	commitments := make([]string, 0, len(claims))
	for _, claim := range claims {
		if claim.Commitment != "" {
			commitments = append(commitments, claim.Commitment)
		}
	}
	sort.Strings(commitments)
	data, _ := json.Marshal(commitments)
	return "sha256:" + hashBytes(data)
}

func decisionReason(foundIntent bool, policy IntentPolicyEnvelope, mode, autonomy string, allowAction bool) string {
	switch {
	case !foundIntent:
		return "intent_missing"
	case policy.NeedsUserAnswer:
		return "critical_slots_missing"
	case !policy.MayAct:
		if mode == "act_gate" && autonomy == "bounded_act" && !allowAction {
			return "action_authority_required"
		}
		if autonomy == "suggest_then_confirm" {
			return "confirmation_required"
		}
		return "autonomy_blocks_action"
	default:
		return "ready"
	}
}

func shouldRedactClaim(disclosure IntentDisclosure, sensitivity string) bool {
	if disclosure.AllowSensitive {
		return false
	}
	switch sensitivity {
	case "private", "restricted":
		return true
	default:
		return false
	}
}

func allowClaimValues(disclosure IntentDisclosure) bool {
	return disclosure.AllowValues != nil && *disclosure.AllowValues
}

func allowCommitments(disclosure IntentDisclosure) bool {
	return disclosure.AllowCommitments != nil && *disclosure.AllowCommitments && allowClaimValues(disclosure) && disclosure.Mode != "existence"
}

func existenceClaimEnvelope(claim ClaimOut) IntentClaimEnvelope {
	return IntentClaimEnvelope{
		Kind:   claim.Kind,
		Status: claim.Status,
	}
}

func claimSensitivity(kind string) string {
	switch {
	case kind == "preference.agent.imported_memory":
		return "restricted"
	case strings.HasPrefix(kind, "boundary."):
		return "private"
	case strings.HasPrefix(kind, "preference.dating."):
		return "private"
	case kind == "preference.food.allergy" || kind == "preference.food.dietary_restriction":
		return "private"
	case strings.HasPrefix(kind, "preference.agent."), strings.HasPrefix(kind, "preference.project."), strings.HasPrefix(kind, "preference."), strings.HasPrefix(kind, "context."):
		return "normal"
	default:
		return "restricted"
	}
}

func hasCriticalUnresolved(rows []IntentQuestion) bool {
	for _, row := range rows {
		if row.Priority == "critical" {
			return true
		}
	}
	return false
}

func hasRedactedCriticalClaim(intentKind string, redacted []string) bool {
	if len(redacted) == 0 {
		return false
	}
	critical := criticalClaimKinds(intentKind)
	for _, kind := range redacted {
		if critical[kind] {
			return true
		}
	}
	return false
}

func criticalClaimKinds(intentKind string) map[string]bool {
	out := map[string]bool{}
	for _, question := range unresolvedIntentQuestions(intentKind, nil) {
		if question.Priority == "critical" {
			out[question.Kind] = true
		}
	}
	return out
}

func profileHash(response IntentResponseEnvelope) string {
	response.RequestID = ""
	response.ProfileHash = ""
	response.SnapshotID = ""
	response.Commitments.ResponseHash = ""
	response.Commitments.SnapshotID = ""
	data, _ := json.Marshal(response)
	return "sha256:" + hashBytes(data)
}

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
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
			fmt.Fprintf(&b, "- %s: %s (confidence %.2f)\n", claim.Kind, terminalSafeInline(claim.Value), claim.Confidence)
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
			fmt.Printf("%-10s %-8s %-7s %s\n", terminalSafeInline(source.ID), terminalSafeInline(source.Kind), terminalSafeInline(source.Health), terminalSafeInline(source.Locator))
			if source.LastError != "" {
				fmt.Printf("  warning: %s\n", terminalSafeInline(source.LastError))
			}
		}
	case IngestResult:
		fmt.Printf("ingested %s: artifacts=%d evidence=%d claims=%d\n", v.Kind, v.Artifacts, v.Evidence, v.Claims)
	case AskResult:
		fmt.Printf("intent: %s (%s)\n", v.Intent.Title, v.Intent.Kind)
		fmt.Println("claims:")
		for _, claim := range v.Claims {
			fmt.Printf("- [%s] %s (%.2f)\n", claim.Kind, terminalSafeInline(claim.Value), claim.Confidence)
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
				fmt.Printf(" warning=%s", terminalSafeInline(coverage.Error))
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
			fmt.Printf("- [%s] %s (%.2f)\n", claim.Kind, terminalSafeInline(claim.Value), claim.Confidence)
		}
	case EditResult:
		fmt.Printf("edited %s -> %s\n", v.ClaimID, terminalSafeInline(v.Value))
	case ExportResult:
		fmt.Printf("exported %s records=%d\n", terminalSafeInline(v.Path), v.Records)
	case DoctorResult:
		fmt.Printf("doctor: %s db=%s schema=%d\n", terminalSafeInline(v.OverallState), terminalSafeInline(v.DBPath), v.Schema)
		for _, check := range v.Checks {
			fmt.Printf("- %s: %s %s\n", check.ID, check.Status, terminalSafeInline(check.Message))
		}
	case []EvidenceOut:
		for _, ev := range v {
			fmt.Printf("- %s %s %s\n", ev.SourceID, ev.ID, terminalSafeInline(ev.Snippet))
		}
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	return nil
}

func terminalSafeInline(text string) string {
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		case '\x1b', '\x7f':
			return -1
		}
		if r < 0x20 || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
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
	case strings.Contains(q, "new job"):
		return "work.new_job"
	case strings.Contains(q, "dating") || hasDatingPhrase(q):
		return "social.dating"
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

func rawTokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range wordRE.FindAllString(strings.ToLower(text), -1) {
		token = strings.Trim(token, "_")
		if token != "" {
			out[token] = true
		}
	}
	return out
}

func hasDatingPhrase(text string) bool {
	tokens := wordTokens(text)
	return hasTokenSequence(tokens, "first", "dates") ||
		hasTokenSequence(tokens, "first", "date") ||
		hasTokenSequence(tokens, "better", "dates") ||
		hasTokenSequence(tokens, "better", "date") ||
		hasTokenSequence(tokens, "plan", "date") ||
		hasTokenSequence(tokens, "date", "ideas") ||
		hasTokenSequence(tokens, "date", "someone") ||
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
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
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

func collectAssistantTexts(value any) []string {
	return collectAssistantTextsValue(value, nil)
}

func collectAssistantTextsValue(value any, out []string) []string {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			out = collectAssistantTextsValue(item, out)
		}
	case map[string]any:
		if role, ok := assistantMessageRole(v); ok {
			if !userAssistantExportRole(role) {
				return out
			}
			out = collectAssistantMessageText(v, out)
			if memory, ok := lookupMapValue(v, "memory"); ok {
				out = collectTexts(memory, out)
			}
			return out
		}
		if memory, ok := lookupMapValue(v, "memory"); ok {
			out = collectTexts(memory, out)
		}
		if message, ok := lookupMapValue(v, "message"); ok {
			out = collectAssistantTextsValue(message, out)
		}
		for _, key := range []string{"messages", "chat_messages", "mapping", "conversations", "items", "children", "payload"} {
			if item, ok := lookupMapValue(v, key); ok {
				if key == "mapping" {
					if mapping, ok := item.(map[string]any); ok {
						keys := make([]string, 0, len(mapping))
						for mappingKey := range mapping {
							keys = append(keys, mappingKey)
						}
						sort.Strings(keys)
						for _, mappingKey := range keys {
							out = collectAssistantTextsValue(mapping[mappingKey], out)
						}
						continue
					}
				}
				out = collectAssistantTextsValue(item, out)
			}
		}
	}
	return out
}

func assistantMessageRole(v map[string]any) (string, bool) {
	for _, key := range []string{"role", "sender"} {
		if item, ok := lookupMapValue(v, key); ok {
			if role, ok := item.(string); ok && strings.TrimSpace(role) != "" {
				return role, true
			}
		}
	}
	if author, ok := lookupMapValue(v, "author"); ok {
		if authorMap, ok := author.(map[string]any); ok {
			if item, ok := lookupMapValue(authorMap, "role"); ok {
				if role, ok := item.(string); ok && strings.TrimSpace(role) != "" {
					return role, true
				}
			}
		}
	}
	return "", false
}

func userAssistantExportRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "human":
		return true
	default:
		return false
	}
}

func collectAssistantMessageText(v map[string]any, out []string) []string {
	for _, key := range []string{"content", "text", "parts"} {
		if item, ok := lookupMapValue(v, key); ok {
			out = collectTexts(item, out)
		}
	}
	return out
}

func lookupMapValue(v map[string]any, key string) (any, bool) {
	for candidate, value := range v {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return nil, false
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
  updated_at text not null,
  revision integer not null default 0
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

create table if not exists revisions (
  id integer primary key autoincrement
);

create table if not exists intent_requests (
  id text primary key,
  uri text not null,
  actor_json text not null default '{}',
  purpose text not null default '',
  audience text not null default 'agent',
  freshness_json text not null default '{}',
  disclosure_json text not null default '{}',
  context_json text not null default '{}',
  request_json text not null default '{}',
  created_at text not null
);

create table if not exists profile_snapshots (
  id text primary key,
  request_id text references intent_requests(id),
  uri text not null,
  resolved_uri text not null,
  intent_id text,
  response_json text not null,
  profile_hash text not null,
  created_at text not null
);

create index if not exists idx_claims_intent_kind on claims(intent_id, kind);
create index if not exists idx_claims_scope on claims(scope);
create index if not exists idx_evidence_artifact on evidence(source_artifact_id);
create index if not exists idx_embeddings_owner on embeddings(owner_kind, owner_id);
create index if not exists idx_intent_requests_uri on intent_requests(uri);
create index if not exists idx_profile_snapshots_hash on profile_snapshots(profile_hash);
`
