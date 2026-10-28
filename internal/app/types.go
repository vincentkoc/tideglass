package app

import (
	"bytes"
	"encoding/json"
	"strings"
)

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
	IntentID         string
	Kind             string
	ForAgent         string
	Budget           int
	ReviewCandidates bool
}

type EditOptions struct {
	ClaimID          string
	Value            string
	Reason           string
	ExpectedRevision *int64
}

type ReviewOptions struct {
	ClaimID          string
	Action           string
	Reason           string
	ExpectedRevision *int64
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
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&decoded); err != nil {
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
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Value        string   `json:"value"`
	Confidence   float64  `json:"confidence"`
	Status       string   `json:"status"`
	SourceMode   string   `json:"source_mode"`
	UpdatedAt    string   `json:"updated_at,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
	ReviewReason string   `json:"review_reason,omitempty"`
	Revision     int64    `json:"-"`
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
	EditID   string `json:"edit_id"`
	ClaimID  string `json:"claim_id"`
	Value    string `json:"value"`
	Revision int64  `json:"revision,omitempty"`
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
