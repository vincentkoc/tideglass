package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

func normalizeIntentRequest(request IntentRequestEnvelope) IntentRequestEnvelope {
	request.SchemaVersion = strings.TrimSpace(request.SchemaVersion)
	if request.SchemaVersion == "" {
		request.SchemaVersion = "tideglass.intent_request.v2"
	}
	request.URI = strings.TrimSpace(request.URI)
	request.Purpose = strings.TrimSpace(request.Purpose)
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
	request.Task.Goal = strings.TrimSpace(request.Task.Goal)
	if request.Task.Goal == "" && request.Purpose != "" {
		request.Task.Goal = request.Purpose
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
	sort.Strings(request.Proof.ZKPredicates)
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
	if len(request.Proof.ZKPredicates) > 0 {
		return errors.New("proof.zk_predicates are not supported")
	}
	if request.Purpose != "" && request.Task.Goal != "" && request.Purpose != request.Task.Goal {
		return errors.New("purpose must match task.goal")
	}
	if request.Proof.Hash != "response" {
		return fmt.Errorf("unsupported proof.hash %q", request.Proof.Hash)
	}
	if request.Proof.Commitments != "per_claim" {
		return fmt.Errorf("unsupported proof.commitments %q", request.Proof.Commitments)
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

func nonNilMap(value map[string]any) map[string]any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func cloneContextMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}
