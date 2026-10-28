package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

func (t *Tideglass) ResolveIntent(ctx context.Context, opts ResolveOptions) (IntentResponseEnvelope, error) {
	if opts.NoPersist && opts.AllowAction {
		return IntentResponseEnvelope{}, errors.New("action authorization requires persisted audit")
	}
	request := opts.Request
	request.Context = cloneContextMap(request.Context)
	request.URI = strings.TrimSpace(request.URI)
	startRevision, err := currentRevision(ctx, t.db)
	if err != nil {
		return IntentResponseEnvelope{}, err
	}
	kind, audienceFromURI, err := parseIntentURI(request.URI)
	if err != nil {
		return IntentResponseEnvelope{}, err
	}
	if audienceFromURI != "" {
		request.Audience.Type = audienceFromURI
		request.Audience.ID = ""
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
		loadedForPolicy := loaded
		hasScopedActionConstraint := false
		if request.Task.Mode == "act_gate" {
			actionGateClaims, err := t.loadActionGateClaims(ctx, intent.ID, kind, request.Contract.RequiredSlots)
			if err != nil {
				return IntentResponseEnvelope{}, err
			}
			actionGateClaims, _, err = t.attachClaimEvidence(ctx, actionGateClaims)
			if err != nil {
				return IntentResponseEnvelope{}, err
			}
			loadedForPolicy = mergeClaimOuts(loadedForPolicy, actionGateClaims)
			_, _, hasScopedActionConstraint = latestMatchingActionConstraint(kind, actionGateClaims, request)
			claims = eligibleClaimsForRequest(loadedForPolicy, requireReviewed, maxAge)
			policyFailedClaims = policyFailingClaims(loadedForPolicy, claims)
			policyFailedClaims = mergeClaimOuts(policyFailedClaims, actionGatePolicyFailingClaims(kind, actionGateClaims, request, maxAge))
		} else {
			claims = eligibleClaimsForRequest(loadedForPolicy, requireReviewed, maxAge)
			policyFailedClaims = policyFailingClaims(loadedForPolicy, claims)
		}
		slotClaims := claimsForSlotSatisfaction(kind, loadedForPolicy, request, maxAge)
		unresolved = unresolvedIntentQuestions(intent.Kind, slotClaims)
		unresolved = addRequiredSlotQuestions(unresolved, slotClaims, request.Contract.RequiredSlots)
		if request.Task.Mode == "act_gate" && request.Task.Autonomy == "bounded_act" && !hasScopedActionConstraint {
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
	redactedBlocking := redactedBlockingQuestions(kind, policy.Redacted, request.Contract.RequiredSlots, unresolved, request.Task.Mode)
	if len(redactedBlocking) > 0 {
		unresolved = append(unresolved, redactedBlocking...)
	}
	var policyFailedBlocking []IntentQuestion
	if request.Task.Mode == "act_gate" {
		policyFailedBlocking = policyFailedBlockingQuestions(kind, policyFailedClaims, request, unresolved)
		if len(policyFailedBlocking) > 0 {
			unresolved = append(unresolved, policyFailedBlocking...)
		}
	}
	if request.Task.Mode == "act_gate" && len(claims) == 0 && len(policyFailedClaims) > 0 {
		unresolved = append(unresolved, questionForSlot("policy.claim.review_or_freshness", "critical", true))
	}
	if hasCriticalUnresolved(redactedBlocking) || hasRedactedCriticalClaim(kind, policy.Redacted, request.Task.Mode) {
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
	if request.Task.Mode == "act_gate" && request.Task.Autonomy == "suggest_then_confirm" && !policy.NeedsUserAnswer {
		unresolved = append(unresolved, questionForSlot("policy.action.confirmation", "critical", true))
		policy.NeedsUserAnswer = true
		policy.MayAct = false
		policy.CapabilityRequired = append(policy.CapabilityRequired, "user_confirmation")
	}
	endRevision, err := currentRevision(ctx, t.db)
	if err != nil {
		return IntentResponseEnvelope{}, err
	}
	if endRevision != startRevision {
		unresolved = append(unresolved, questionForSlot("policy.action.snapshot", "critical", true))
		policy.NeedsUserAnswer = true
		policy.MayAct = false
		policy.CapabilityRequired = append(policy.CapabilityRequired, "consistent_snapshot")
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
		Links:         map[string]string{},
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
		if response.Decision.MayAct {
			snapshotID, authorized, err := t.persistAuthorizedProfileSnapshot(ctx, requestID, response, startRevision, request, maxAge)
			if err != nil {
				return IntentResponseEnvelope{}, err
			}
			if authorized {
				response.SnapshotID = snapshotID
				if hasCommitments(response.Commitments) {
					response.Commitments.SnapshotID = response.SnapshotID
				}
				return response, nil
			}
			response = failClosedActionSnapshot(response)
		}
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

func failClosedActionSnapshot(response IntentResponseEnvelope) IntentResponseEnvelope {
	response.Unresolved = append(response.Unresolved, questionForSlot("policy.action.snapshot", "critical", true))
	response.Policy.NeedsUserAnswer = true
	response.Policy.MayAct = false
	response.Policy.CapabilityRequired = append(response.Policy.CapabilityRequired, "consistent_snapshot")
	response.Decision.MayAct = false
	response.Decision.NeedsUserAnswer = true
	response.Decision.Reason = "critical_slots_missing"
	response.Status = "partial"
	response.SnapshotID = ""
	response.Commitments.SnapshotID = ""
	response.ProfileHash = profileHash(response)
	if hasCommitments(response.Commitments) {
		response.Commitments.ResponseHash = response.ProfileHash
	}
	return response
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
	return persistProfileSnapshotWith(ctx, t.db, requestID, response)
}

func (t *Tideglass) persistAuthorizedProfileSnapshot(ctx context.Context, requestID string, response IntentResponseEnvelope, startRevision int64, request IntentRequestEnvelope, maxAge time.Duration) (string, bool, error) {
	conn, err := t.db.Conn(ctx)
	if err != nil {
		return "", false, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `begin immediate`); err != nil {
		return "", false, err
	}
	committed := false
	defer func() {
		if !committed {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = conn.ExecContext(cleanupCtx, `rollback`)
		}
	}()
	endRevision, err := currentRevision(ctx, conn)
	if err != nil {
		return "", false, err
	}
	if endRevision != startRevision || !deadlineConstraintValid(request.Task.Deadline, request.Task.Deadline) || responseClaimsStale(response.Claims, maxAge) {
		return "", false, nil
	}
	snapshotID, err := persistProfileSnapshotWith(ctx, conn, requestID, response)
	if err != nil {
		return "", false, err
	}
	if _, err := conn.ExecContext(ctx, `commit`); err != nil {
		return "", false, err
	}
	committed = true
	return snapshotID, true, nil
}

type snapshotExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func persistProfileSnapshotWith(ctx context.Context, execer snapshotExecer, requestID string, response IntentResponseEnvelope) (string, error) {
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
	_, err = execer.ExecContext(ctx, `
insert into profile_snapshots(id,request_id,uri,resolved_uri,intent_id,response_json,profile_hash,created_at)
values(?,?,?,?,?,?,?,?)`,
		snapshotID, requestID, response.URI, response.ResolvedURI, intentID, string(responseJSON), response.ProfileHash, now())
	return snapshotID, err
}

func responseClaimsStale(claims []IntentClaimEnvelope, maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	for _, claim := range claims {
		if claim.FreshAt == "" || claimStale(ClaimOut{UpdatedAt: claim.FreshAt}, maxAge) {
			return true
		}
	}
	return false
}
