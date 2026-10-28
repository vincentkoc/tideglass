package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

func (t *Tideglass) Profile(ctx context.Context, opts ProfileOptions) (ProfileResult, error) {
	intent, err := t.findIntent(ctx, opts.IntentID, opts.Kind)
	if err != nil {
		return ProfileResult{}, err
	}
	claims, err := t.loadClaims(ctx, intent.ID)
	if err != nil {
		return ProfileResult{}, err
	}
	if opts.ReviewCandidates {
		reviewClaims, err := t.loadReviewCandidateClaims(ctx, intent.ID)
		if err != nil {
			return ProfileResult{}, err
		}
		claims = mergeClaimOuts(claims, reviewClaims)
	}
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

func (t *Tideglass) EditClaim(ctx context.Context, opts EditOptions) (EditResult, error) {
	claimID := strings.TrimSpace(opts.ClaimID)
	if claimID == "" {
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
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `select revision from claims where id = ?`, claimID).Scan(&currentRevision); err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	if opts.ExpectedRevision != nil && currentRevision != *opts.ExpectedRevision {
		_ = tx.Rollback()
		return EditResult{}, fmt.Errorf("claim revision changed: got %d, want %d", currentRevision, *opts.ExpectedRevision)
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `insert into edits(id,claim_id,operation,patch_json,reason,created_at) values(?,?,?,?,?,?)`,
		editID, claimID, "supersede", string(patch), opts.Reason, now()); err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	res, err := tx.ExecContext(ctx, `update claims set status = 'active', normalized_value = ?, updated_at = ?, revision = ? where id = ? and revision = ?`, normalizeText(value), now(), revision, claimID, currentRevision)
	if err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return EditResult{}, err
	}
	if affected == 0 {
		_ = tx.Rollback()
		return EditResult{}, fmt.Errorf("claim revision changed: got unknown, want %d", currentRevision)
	}
	if err := tx.Commit(); err != nil {
		return EditResult{}, err
	}
	return EditResult{EditID: editID, ClaimID: claimID, Value: value, Revision: revision}, nil
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
	var currentStatus string
	var currentRevision int64
	if err := tx.QueryRowContext(ctx, `select status, revision from claims where id = ?`, claimID).Scan(&currentStatus, &currentRevision); err != nil {
		_ = tx.Rollback()
		return ReviewResult{}, err
	}
	if opts.ExpectedRevision != nil && currentRevision != *opts.ExpectedRevision {
		_ = tx.Rollback()
		return ReviewResult{}, fmt.Errorf("claim revision changed: got %d, want %d", currentRevision, *opts.ExpectedRevision)
	}
	revision, err := nextRevision(ctx, tx)
	if err != nil {
		_ = tx.Rollback()
		return ReviewResult{}, err
	}
	res, err := tx.ExecContext(ctx, `update claims set status = ?, updated_at = ?, revision = ? where id = ? and revision = ? and status = ?`, status, now(), revision, claimID, currentRevision, currentStatus)
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

func (t *Tideglass) linkClaimEvidence(ctx context.Context, claimID, evidenceID, role string) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `insert or ignore into claim_evidence(claim_id,evidence_id,role) values(?,?,?)`, claimID, evidenceID, role)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if affected == 0 {
		return tx.Commit()
	}
	if _, err := nextRevision(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func currentRevision(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var revision int64
	if err := queryer.QueryRowContext(ctx, `select coalesce(max(id), 0) from revisions`).Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

type observedRevision struct {
	id        string
	revision  int64
	updatedAt string
	ok        bool
}

func recordLatestRevision(revisions map[string]observedRevision, key string, revision int64, updatedAt, id string) {
	current := revisions[key]
	if !current.ok || revision > current.revision || (revision == current.revision && timestampAfter(updatedAt, current.updatedAt)) || (revision == current.revision && updatedAt == current.updatedAt && id > current.id) {
		revisions[key] = observedRevision{id: id, revision: revision, updatedAt: updatedAt, ok: true}
	}
}

func (t *Tideglass) loadClaims(ctx context.Context, intentID string) ([]ClaimOut, error) {
	rows, err := t.db.QueryContext(ctx, `
select c.id, c.kind,
       coalesce(json_extract(e.patch_json, '$.value'), c.value) as value,
       c.confidence, c.status, c.source_mode,
       c.created_at, c.updated_at, coalesce(e.created_at, ''),
       case when e.id is null then 0 else 1 end as has_value_edit,
       c.revision
from claims c
left join edits e on e.id = (
  select id from edits
  where claim_id = c.id and json_extract(patch_json, '$.value') is not null
  order by rowid desc limit 1
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
		if err := rows.Scan(&claim.ID, &claim.Kind, &claim.Value, &claim.Confidence, &claim.Status, &claim.SourceMode, &claim.CreatedAt, &claim.UpdatedAt, &valueEditCreatedAt, &hasValueEdit, &claim.Revision); err != nil {
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
       c.normalized_value, c.confidence, c.status, c.source_mode,
       c.updated_at, coalesce(e.created_at, ''), c.revision
from claims c
left join edits e on e.id = (
  select id from edits
  where claim_id = c.id and json_extract(patch_json, '$.value') is not null
  order by rowid desc limit 1
)
where c.intent_id = ?
order by c.created_at desc`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []ClaimOut
	bestAcceptedSingleton := map[string]observedRevision{}
	actionGateClaimRevisions := map[string]int64{}
	bestAcceptedDuplicate := map[string]observedRevision{}
	bestRejectedDuplicate := map[string]observedRevision{}
	actionGateClaimKeys := map[string]string{}
	for rows.Next() {
		var claim ClaimOut
		var normalized string
		var valueEditCreatedAt string
		var revision int64
		if err := rows.Scan(&claim.ID, &claim.Kind, &claim.Value, &normalized, &claim.Confidence, &claim.Status, &claim.SourceMode, &claim.UpdatedAt, &valueEditCreatedAt, &revision); err != nil {
			return nil, err
		}
		if !blocking[claim.Kind] && !strings.HasPrefix(claim.Kind, "boundary.") && !actionConstraintClaimKind(claim.Kind) {
			continue
		}
		if timestampAfter(valueEditCreatedAt, claim.UpdatedAt) {
			claim.UpdatedAt = valueEditCreatedAt
		}
		claim.Revision = revision
		normalized = losslessClaimValueKey(claim.Value)
		duplicateKey := claim.Kind + "\x00" + normalized
		if singletonClaimKind(claim.Kind) && claim.Status == "accepted" {
			recordLatestRevision(bestAcceptedSingleton, claim.Kind, revision, claim.UpdatedAt, claim.ID)
		}
		if claim.Status == "accepted" {
			recordLatestRevision(bestAcceptedDuplicate, duplicateKey, revision, claim.UpdatedAt, claim.ID)
		}
		if claim.Status == "rejected" {
			recordLatestRevision(bestRejectedDuplicate, duplicateKey, revision, claim.UpdatedAt, claim.ID)
		}
		claims = append(claims, claim)
		actionGateClaimRevisions[claim.ID] = revision
		actionGateClaimKeys[claim.ID] = duplicateKey
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ClaimOut, 0, len(claims))
	for _, claim := range claims {
		revision := actionGateClaimRevisions[claim.ID]
		duplicateAcceptedRevision := bestAcceptedDuplicate[actionGateClaimKeys[claim.ID]]
		if duplicateAcceptedRevision.ok && (claim.Status != "accepted" || revision < duplicateAcceptedRevision.revision) {
			continue
		}
		if duplicateAcceptedRevision.ok && claim.Status == "accepted" && revision == duplicateAcceptedRevision.revision {
			if timestampAfter(duplicateAcceptedRevision.updatedAt, claim.UpdatedAt) || (duplicateAcceptedRevision.updatedAt == claim.UpdatedAt && claim.ID != duplicateAcceptedRevision.id) {
				continue
			}
		}
		duplicateRejectedRevision := bestRejectedDuplicate[actionGateClaimKeys[claim.ID]]
		if duplicateRejectedRevision.ok && claim.Status != "accepted" {
			if claim.Status == "rejected" {
				continue
			}
			if revision < duplicateRejectedRevision.revision || (revision == duplicateRejectedRevision.revision && !timestampAfter(claim.UpdatedAt, duplicateRejectedRevision.updatedAt)) {
				continue
			}
		}
		singletonAcceptedRevision := bestAcceptedSingleton[claim.Kind]
		if singletonClaimKind(claim.Kind) && singletonAcceptedRevision.ok && claim.Status == "accepted" {
			if revision < singletonAcceptedRevision.revision || (revision == singletonAcceptedRevision.revision && timestampAfter(singletonAcceptedRevision.updatedAt, claim.UpdatedAt)) {
				continue
			}
			if revision == singletonAcceptedRevision.revision && singletonAcceptedRevision.updatedAt == claim.UpdatedAt && claim.ID != singletonAcceptedRevision.id {
				claim.Status = "active"
			}
		}
		if singletonClaimKind(claim.Kind) && singletonAcceptedRevision.ok && claim.Status != "accepted" {
			if revision < singletonAcceptedRevision.revision || (revision == singletonAcceptedRevision.revision && timestampAfter(singletonAcceptedRevision.updatedAt, claim.UpdatedAt)) {
				continue
			}
		}
		out = append(out, claim)
	}
	return out, nil
}

func (t *Tideglass) loadReviewCandidateClaims(ctx context.Context, intentID string) ([]ClaimOut, error) {
	rows, err := t.db.QueryContext(ctx, `
select c.id, c.kind,
       coalesce(json_extract(e.patch_json, '$.value'), c.value) as value,
       c.confidence, c.status, c.source_mode,
       c.created_at, c.updated_at, coalesce(e.created_at, ''),
       case when e.id is null then 0 else 1 end as has_value_edit,
       c.revision
from claims c
left join edits e on e.id = (
  select id from edits
  where claim_id = c.id and json_extract(patch_json, '$.value') is not null
  order by rowid desc limit 1
)
where c.intent_id = ?
order by c.created_at desc`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type materializedReviewClaim struct {
		ClaimOut
		CreatedAt    string
		UpdatedAt    string
		HasValueEdit bool
		Normalized   string
	}
	var rowsOut []materializedReviewClaim
	for rows.Next() {
		var claim materializedReviewClaim
		var valueEditCreatedAt string
		var hasValueEdit int
		var revision int64
		if err := rows.Scan(&claim.ID, &claim.Kind, &claim.Value, &claim.Confidence, &claim.Status, &claim.SourceMode, &claim.CreatedAt, &claim.UpdatedAt, &valueEditCreatedAt, &hasValueEdit, &revision); err != nil {
			return nil, err
		}
		if !singletonClaimKind(claim.Kind) {
			continue
		}
		if timestampAfter(valueEditCreatedAt, claim.UpdatedAt) {
			claim.UpdatedAt = valueEditCreatedAt
		}
		claim.Revision = revision
		claim.HasValueEdit = hasValueEdit == 1
		claim.ClaimOut.UpdatedAt = claim.UpdatedAt
		claim.Normalized = losslessClaimValueKey(claim.Value)
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
	prefer := func(left, right materializedReviewClaim) bool {
		if decisionRank(left.Status) != decisionRank(right.Status) {
			return decisionRank(left.Status) > decisionRank(right.Status)
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
		return prefer(rowsOut[leftIndex], rowsOut[rightIndex])
	})
	duplicateSeen := map[string]bool{}
	var deduped []materializedReviewClaim
	for _, claim := range rowsOut {
		key := claim.Kind + "\x00" + claim.Normalized
		if duplicateSeen[key] {
			continue
		}
		duplicateSeen[key] = true
		deduped = append(deduped, claim)
	}
	bestAcceptedSingleton := map[string]observedRevision{}
	for _, claim := range deduped {
		if claim.Status == "accepted" {
			recordLatestRevision(bestAcceptedSingleton, claim.Kind, claim.Revision, claim.UpdatedAt, claim.ID)
		}
	}
	out := make([]ClaimOut, 0, len(deduped))
	for _, claim := range deduped {
		acceptedRevision := bestAcceptedSingleton[claim.Kind]
		if claim.Status == "accepted" && acceptedRevision.ok && claim.ID != acceptedRevision.id && claim.Revision == acceptedRevision.revision && claim.UpdatedAt == acceptedRevision.updatedAt {
			claim.ReviewReason = "conflict"
			out = append(out, claim.ClaimOut)
			continue
		}
		if claim.Status != "active" {
			continue
		}
		if acceptedRevision.ok {
			if claim.Revision < acceptedRevision.revision || (claim.Revision == acceptedRevision.revision && timestampAfter(acceptedRevision.updatedAt, claim.UpdatedAt)) {
				continue
			}
		}
		out = append(out, claim.ClaimOut)
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

func mergeClaimOuts(primary []ClaimOut, extra []ClaimOut) []ClaimOut {
	seen := map[string]int{}
	out := make([]ClaimOut, 0, len(primary)+len(extra))
	for _, claim := range primary {
		seen[claim.ID] = len(out)
		out = append(out, claim)
	}
	for _, claim := range extra {
		if index, ok := seen[claim.ID]; ok {
			if out[index].ReviewReason == "" && claim.ReviewReason != "" {
				out[index].ReviewReason = claim.ReviewReason
			}
			continue
		}
		seen[claim.ID] = len(out)
		out = append(out, claim)
	}
	return out
}
