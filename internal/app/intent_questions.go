package app

import (
	"strings"
)

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

func redactedBlockingQuestions(intentKind string, redacted []string, requiredSlots []string, existing []IntentQuestion, taskMode string) []IntentQuestion {
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
		if actionConstraintClaimKind(kind) && taskMode != "act_gate" {
			continue
		}
		if (blocking[kind] || strings.HasPrefix(kind, "boundary.") || actionConstraintClaimKind(kind)) && !haveBlockingQuestions[kind] {
			out = append(out, questionForSlot(kind, "critical", true))
			haveBlockingQuestions[kind] = true
		}
	}
	return out
}

func policyFailedBlockingQuestions(intentKind string, claims []ClaimOut, request IntentRequestEnvelope, existing []IntentQuestion) []IntentQuestion {
	if len(claims) == 0 {
		return nil
	}
	blocking := criticalClaimKinds(intentKind)
	for _, slot := range request.Contract.RequiredSlots {
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
		slot := claim.Kind
		if !actionConstraintClaimKind(claim.Kind) && shouldRedactClaim(request.Disclosure, claimSensitivity(claim.Kind)) {
			slot = "policy.sensitive_disclosure"
		}
		if (blocking[claim.Kind] || strings.HasPrefix(claim.Kind, "boundary.") || actionConstraintClaimKind(claim.Kind)) && !haveBlockingQuestions[slot] {
			out = append(out, questionForSlot(slot, "critical", true))
			haveBlockingQuestions[slot] = true
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
	case "policy.action.confirmation":
		return "Confirm this suggested action before Tideglass treats it as approved."
	case "policy.action.snapshot":
		return "Retry after Tideglass resolves a consistent action-policy snapshot."
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
