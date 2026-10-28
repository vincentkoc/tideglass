package app

import (
	"context"
	"database/sql"
	"strings"

	"github.com/openclaw/crawlkit/store"
)

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

func sqliteDuplicateColumnError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column")
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
			if !sqliteDuplicateColumnError(err) {
				return err
			}
		}
	}
	if _, err := t.db.ExecContext(ctx, `create table if not exists revisions (id integer primary key autoincrement)`); err != nil {
		return err
	}
	if _, err := t.db.ExecContext(ctx, `create table if not exists maintenance_tasks (name text primary key, applied_at text not null)`); err != nil {
		return err
	}
	if _, err := t.db.ExecContext(ctx, `create index if not exists idx_edits_claim_id on edits(claim_id)`); err != nil {
		return err
	}
	if _, err := t.db.ExecContext(ctx, `create index if not exists idx_edits_claim_operation on edits(claim_id, operation)`); err != nil {
		return err
	}
	if !sqliteColumnExists(ctx, t.db, "claims", "revision") {
		if _, err := t.db.ExecContext(ctx, `alter table claims add column revision integer not null default 0`); err != nil {
			if !sqliteDuplicateColumnError(err) {
				return err
			}
		}
	}
	return t.reconcileLegacyReviewedEditsOnce(ctx)
}

func (t *Tideglass) reconcileLegacyReviewedEditsOnce(ctx context.Context) error {
	tx, err := t.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.ExecContext(ctx, `insert or ignore into maintenance_tasks(name, applied_at) values(?, ?)`, "legacy_reviewed_edit_reconciliation_v1", now())
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		if err := tx.Commit(); err != nil {
			return err
		}
		committed = true
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
update claims
set status = 'active',
    updated_at = coalesce((
      select max(created_at) from edits
      where edits.claim_id = claims.id and json_extract(patch_json, '$.value') is not null
    ), updated_at)
where status in ('accepted', 'rejected')
  and exists (
    select 1 from edits value_edits
    where value_edits.claim_id = claims.id
      and json_extract(value_edits.patch_json, '$.value') is not null
      and value_edits.rowid = (
        select latest_value.rowid from edits latest_value
        where latest_value.claim_id = claims.id
          and json_extract(latest_value.patch_json, '$.value') is not null
        order by latest_value.rowid desc limit 1
      )
      and (
        julianday(value_edits.created_at) > coalesce((
          select max(julianday(review_edits.created_at)) from edits review_edits
          where review_edits.claim_id = claims.id and review_edits.operation in ('accept', 'reject')
        ), 0)
        or (
          julianday(value_edits.created_at) = coalesce((
            select max(julianday(review_edits.created_at)) from edits review_edits
            where review_edits.claim_id = claims.id and review_edits.operation in ('accept', 'reject')
          ), 0)
          and value_edits.rowid > coalesce((
            select latest_review.rowid from edits latest_review
            where latest_review.claim_id = claims.id and latest_review.operation in ('accept', 'reject')
            order by latest_review.rowid desc limit 1
          ), 0)
        )
      )
  )`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
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

create table if not exists maintenance_tasks (
  name text primary key,
  applied_at text not null
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
create index if not exists idx_edits_claim_id on edits(claim_id);
create index if not exists idx_edits_claim_operation on edits(claim_id, operation);
create index if not exists idx_evidence_artifact on evidence(source_artifact_id);
create index if not exists idx_embeddings_owner on embeddings(owner_kind, owner_id);
create index if not exists idx_intent_requests_uri on intent_requests(uri);
create index if not exists idx_profile_snapshots_hash on profile_snapshots(profile_hash);
`
