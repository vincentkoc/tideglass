package app

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

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
		if actionConstraintClaimKind(claim.Kind) {
			continue
		}
		if !eligibleByID[claim.ID] {
			out = append(out, claim)
		}
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

func losslessClaimValueKey(value string) string {
	return strings.TrimSpace(value)
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
	existenceSeen := map[string]bool{}
	scopeByRelevance := request.Disclosure.Mode == "minimal" || !audienceIsLocal(request.Audience) || !audienceShareTargetsAreLocal(request)
	addCapability := func(capability string) {
		if !stringSliceContains(policy.CapabilityRequired, capability) {
			policy.CapabilityRequired = append(policy.CapabilityRequired, capability)
		}
	}
	for _, claim := range claims {
		if request.Contract.ConfidenceFloor > 0 && claim.Confidence < request.Contract.ConfidenceFloor {
			continue
		}
		sensitivity := claimSensitivity(claim.Kind)
		if scopeByRelevance && !claimRelevantToRequest(claim.Kind, request) && !claimRequiredForActionGate(intentKind, claim.Kind, request) {
			if omittedClaimBlocksReadiness(intentKind, claim.Kind, request) {
				if shouldRedactClaim(request.Disclosure, sensitivity) {
					policy.NeedsUserAnswer = true
					addCapability("sensitive_disclosure")
				} else {
					redacted[claim.Kind] = true
				}
			}
			continue
		}
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
			policy.NeedsUserAnswer = true
			addCapability("sensitive_disclosure")
			continue
		}
		if request.Disclosure.Mode == "existence" {
			key := claim.Kind + "\x00" + claim.Status
			if !existenceSeen[key] {
				out = append(out, existenceClaimEnvelope(claim))
				existenceSeen[key] = true
			}
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
	if request.Task.Mode == "act_gate" && request.Task.Autonomy == "bounded_act" && allowAction && !policy.NeedsUserAnswer && allowClaimValues(request.Disclosure) && request.Disclosure.Mode != "existence" {
		policy.MayAct = true
	}
	return out, policy
}

func omittedClaimBlocksReadiness(intentKind, kind string, request IntentRequestEnvelope) bool {
	if criticalClaimKinds(intentKind)[kind] {
		return true
	}
	for _, slot := range request.Contract.RequiredSlots {
		if kind == strings.TrimSpace(slot) {
			return true
		}
	}
	return false
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
	if !audienceShareTargetsAreLocal(request) {
		return false
	}
	return audienceIsLocal(request.Audience)
}

func audienceShareTargetsAreLocal(request IntentRequestEnvelope) bool {
	for _, target := range request.Audience.ShareWith {
		target = strings.TrimSpace(target)
		if target == "" || target == "agent" || target == "local" {
			continue
		}
		return false
	}
	return true
}

func audienceIsLocal(audience IntentAudience) bool {
	audienceType := strings.TrimSpace(audience.Type)
	if audienceType != "agent" && audienceType != "local" {
		return false
	}
	audienceID := strings.TrimSpace(audience.ID)
	return audienceID == "" || audienceID == "agent" || audienceID == "local"
}

func decisionReason(foundIntent bool, policy IntentPolicyEnvelope, mode, autonomy string, allowAction bool) string {
	switch {
	case !foundIntent:
		return "intent_missing"
	case policy.NeedsUserAnswer:
		if mode == "act_gate" && autonomy == "suggest_then_confirm" && stringSliceContains(policy.CapabilityRequired, "user_confirmation") {
			return "confirmation_required"
		}
		return "critical_slots_missing"
	case !policy.MayAct:
		if mode == "act_gate" && autonomy == "bounded_act" && !allowAction {
			return "action_authority_required"
		}
		if mode == "act_gate" && autonomy == "suggest_then_confirm" {
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
	case strings.HasPrefix(kind, "preference.agent."), strings.HasPrefix(kind, "preference.project."):
		return "normal"
	case strings.HasPrefix(kind, "preference."), strings.HasPrefix(kind, "context."):
		return "restricted"
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

func hasRedactedCriticalClaim(intentKind string, redacted []string, taskMode string) bool {
	if len(redacted) == 0 {
		return false
	}
	critical := criticalClaimKinds(intentKind)
	for _, kind := range redacted {
		if actionConstraintClaimKind(kind) && taskMode != "act_gate" {
			continue
		}
		if critical[kind] || actionConstraintClaimKind(kind) {
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
