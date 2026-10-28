package app

import (
	"encoding/json"
	"sort"
)

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

func profileHash(response IntentResponseEnvelope) string {
	response.RequestID = ""
	response.ProfileHash = ""
	response.SnapshotID = ""
	response.Commitments.ResponseHash = ""
	response.Commitments.SnapshotID = ""
	data, _ := json.Marshal(response)
	return "sha256:" + hashBytes(data)
}
