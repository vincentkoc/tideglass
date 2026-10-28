package app

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openclaw/crawlkit/store"
)

const schemaVersion = 3

type Tideglass struct {
	store *store.Store
	db    *sql.DB
	path  string
}

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
