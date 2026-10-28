package app

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

func actionGatePolicyFailingClaims(intentKind string, claims []ClaimOut, request IntentRequestEnvelope, maxAge time.Duration) []ClaimOut {
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range request.Contract.RequiredSlots {
		if strings.TrimSpace(slot) != "" {
			blocking[strings.TrimSpace(slot)] = true
		}
	}
	var out []ClaimOut
	if latestClaim, latestConstraint, ok := latestMatchingActionConstraint(intentKind, claims, request); ok {
		if latestClaim.Status != "accepted" || claimStale(latestClaim, maxAge) || belowConfidenceFloor(latestClaim, request) || !latestConstraint.Allow {
			out = append(out, latestClaim)
		}
		for _, claim := range claims {
			if claim.ID == latestClaim.ID || claim.Revision != latestClaim.Revision || claim.UpdatedAt != latestClaim.UpdatedAt {
				continue
			}
			constraint, ok := matchingActionConstraint(intentKind, claim, request)
			if !ok {
				continue
			}
			if claim.Status != "accepted" || claimStale(claim, maxAge) || belowConfidenceFloor(claim, request) || !constraint.Allow {
				out = append(out, claim)
			}
		}
	}
	for _, claim := range claims {
		if actionConstraintClaimKind(claim.Kind) {
			if _, ok := decodeActionConstraint(claim); !ok {
				out = append(out, claim)
			}
			continue
		}
		if !blocking[claim.Kind] && !strings.HasPrefix(claim.Kind, "boundary.") {
			continue
		}
		if claim.Status != "accepted" || claimStale(claim, maxAge) || belowConfidenceFloor(claim, request) {
			out = append(out, claim)
		}
	}
	return out
}

type actionConstraintValue struct {
	Allow        bool     `json:"allow"`
	IntentKind   string   `json:"intent_kind"`
	TaskMode     string   `json:"task_mode"`
	Autonomy     string   `json:"autonomy"`
	Goal         string   `json:"goal"`
	AudienceType string   `json:"audience_type"`
	AudienceID   string   `json:"audience_id,omitempty"`
	ShareWith    []string `json:"share_with,omitempty"`
	Deadline     string   `json:"deadline"`
	ScopeHash    string   `json:"scope_hash"`
}

func latestMatchingActionConstraint(intentKind string, claims []ClaimOut, request IntentRequestEnvelope) (ClaimOut, actionConstraintValue, bool) {
	var latestClaim ClaimOut
	var latestConstraint actionConstraintValue
	var found bool
	for _, claim := range claims {
		constraint, ok := matchingActionConstraint(intentKind, claim, request)
		if !ok {
			continue
		}
		if !found || claimNewer(claim, latestClaim) {
			latestClaim = claim
			latestConstraint = constraint
			found = true
		}
	}
	return latestClaim, latestConstraint, found
}

func matchingActionConstraint(intentKind string, claim ClaimOut, request IntentRequestEnvelope) (actionConstraintValue, bool) {
	constraint, ok := decodeActionConstraint(claim)
	if !ok {
		return actionConstraintValue{}, false
	}
	return actionConstraintMatchesRequest(intentKind, constraint, request)
}

func decodeActionConstraint(claim ClaimOut) (actionConstraintValue, bool) {
	if !actionConstraintClaimKind(claim.Kind) {
		return actionConstraintValue{}, false
	}
	var constraint actionConstraintValue
	dec := json.NewDecoder(strings.NewReader(claim.Value))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&constraint); err != nil {
		return actionConstraintValue{}, false
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return actionConstraintValue{}, false
	}
	return constraint, true
}

func actionConstraintMatchesRequest(intentKind string, constraint actionConstraintValue, request IntentRequestEnvelope) (actionConstraintValue, bool) {
	if normalizeKind(constraint.IntentKind, "") != intentKind || strings.TrimSpace(constraint.IntentKind) == "" {
		return actionConstraintValue{}, false
	}
	if !sameRequiredEnumConstraintValue(constraint.TaskMode, request.Task.Mode) {
		return actionConstraintValue{}, false
	}
	if !sameRequiredEnumConstraintValue(constraint.Autonomy, request.Task.Autonomy) {
		return actionConstraintValue{}, false
	}
	if !sameRequiredExactConstraintValue(constraint.Goal, request.Task.Goal) {
		return actionConstraintValue{}, false
	}
	if !sameRequiredExactConstraintValue(constraint.AudienceType, request.Audience.Type) {
		return actionConstraintValue{}, false
	}
	if strings.TrimSpace(constraint.AudienceID) != strings.TrimSpace(request.Audience.ID) {
		return actionConstraintValue{}, false
	}
	if !sameStringSet(constraint.ShareWith, request.Audience.ShareWith) {
		return actionConstraintValue{}, false
	}
	if !deadlineConstraintValid(constraint.Deadline, request.Task.Deadline) {
		return actionConstraintValue{}, false
	}
	if !sameRequiredExactConstraintValue(constraint.ScopeHash, actionScopeHash(request)) {
		return actionConstraintValue{}, false
	}
	return constraint, true
}

func claimNewer(left, right ClaimOut) bool {
	if left.Revision != right.Revision {
		return left.Revision > right.Revision
	}
	if timestampAfter(left.UpdatedAt, right.UpdatedAt) {
		return true
	}
	if timestampAfter(right.UpdatedAt, left.UpdatedAt) {
		return false
	}
	return left.ID > right.ID
}

func sameRequiredEnumConstraintValue(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && strings.EqualFold(left, right)
}

func sameRequiredExactConstraintValue(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && left == right
}

func sameStringSet(left, right []string) bool {
	leftValues := normalizedStringSet(left)
	rightValues := normalizedStringSet(right)
	if len(leftValues) != len(rightValues) {
		return false
	}
	for index := range leftValues {
		if leftValues[index] != rightValues[index] {
			return false
		}
	}
	return true
}

func normalizedStringSet(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func deadlineConstraintValid(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" || left != right {
		return false
	}
	deadline, err := time.Parse(time.RFC3339Nano, left)
	if err != nil {
		return false
	}
	return time.Now().Before(deadline)
}

func actionScopeHash(request IntentRequestEnvelope) string {
	scope := map[string]any{
		"actor": map[string]any{
			"type":         request.Actor.Type,
			"id":           request.Actor.ID,
			"session":      request.Actor.Session,
			"trust_tier":   request.Actor.TrustTier,
			"capabilities": normalizedStringSet(request.Actor.Capabilities),
		},
		"task": map[string]any{
			"goal":     request.Task.Goal,
			"mode":     request.Task.Mode,
			"stakes":   request.Task.Stakes,
			"autonomy": request.Task.Autonomy,
			"deadline": request.Task.Deadline,
		},
		"purpose": request.Purpose,
		"audience": map[string]any{
			"type":       request.Audience.Type,
			"id":         request.Audience.ID,
			"share_with": normalizedStringSet(request.Audience.ShareWith),
		},
		"contract": map[string]any{
			"output":           request.Contract.Output,
			"required_slots":   normalizedStringSet(request.Contract.RequiredSlots),
			"optional_slots":   normalizedStringSet(request.Contract.OptionalSlots),
			"confidence_floor": request.Contract.ConfidenceFloor,
			"ask_strategy":     request.Contract.AskStrategy,
		},
		"freshness": map[string]any{
			"max_age":                       request.Freshness.MaxAge,
			"require_reviewed":              request.Freshness.RequireReviewed,
			"accept_inferred_for_questions": request.Freshness.AcceptInferredForQuestions,
		},
		"disclosure": map[string]any{
			"mode":              request.Disclosure.Mode,
			"allow_values":      request.Disclosure.AllowValues,
			"allow_evidence":    request.Disclosure.AllowEvidence,
			"allow_sensitive":   request.Disclosure.AllowSensitive,
			"allow_commitments": request.Disclosure.AllowCommitments,
		},
		"proof": map[string]any{
			"hash":          request.Proof.Hash,
			"commitments":   request.Proof.Commitments,
			"zk_predicates": normalizedStringSet(request.Proof.ZKPredicates),
		},
		"context": actionScopeContext(request.Context),
	}
	data, _ := json.Marshal(scope)
	return "sha256:" + hashBytes(data)
}

func actionScopeContext(contextMap map[string]any) map[string]any {
	if len(contextMap) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(contextMap))
	for key, value := range contextMap {
		if key == "server_authority" || key == "client_request_id" {
			continue
		}
		out[key] = value
	}
	return out
}

func stringSliceContains(rows []string, value string) bool {
	for _, row := range rows {
		if row == value {
			return true
		}
	}
	return false
}

func claimRequiredForActionGate(intentKind, kind string, request IntentRequestEnvelope) bool {
	return request.Task.Mode == "act_gate" && (strings.HasPrefix(kind, "boundary.") || criticalClaimKinds(intentKind)[kind] || actionConstraintClaimKind(kind))
}

func actionConstraintClaimKind(kind string) bool {
	return kind == "policy.action.constraints"
}
