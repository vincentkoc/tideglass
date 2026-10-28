package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openclaw/crawlkit/store"
)

func (t *Tideglass) Sources(ctx context.Context, opts SourceOptions) (SourceList, error) {
	sources := discoverStaticSources()
	crawlbarSources, _ := discoverCrawlBar(ctx)
	sourceByID := map[string]SourceStatus{}
	for _, src := range sources {
		sourceByID[src.ID] = src
	}
	for _, src := range crawlbarSources {
		if existing, ok := sourceByID[src.ID]; ok {
			existing.Label = src.Label
			if strings.TrimSpace(src.Locator) != "" {
				existing.Locator = src.Locator
			}
			existing.Metadata = mergeStringMaps(existing.Metadata, src.Metadata)
			sourceByID[src.ID] = existing
			continue
		}
		sourceByID[src.ID] = src
	}
	out := make([]SourceStatus, 0, len(sourceByID))
	for _, source := range sourceByID {
		source, err := t.mergePersistedSource(ctx, source)
		if err != nil {
			return SourceList{}, err
		}
		if opts.Probe {
			source = probeSource(ctx, source)
		}
		if err := t.upsertSource(ctx, source); err != nil {
			return SourceList{}, err
		}
		out = append(out, source)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return SourceList{Sources: out}, nil
}

func (t *Tideglass) upsertSource(ctx context.Context, source SourceStatus) error {
	caps, _ := json.Marshal(source.Capabilities)
	counts, _ := json.Marshal(source.Counts)
	meta, _ := json.Marshal(source.Metadata)
	_, err := t.db.ExecContext(ctx, `
insert into sources(id,kind,label,locator,health,capabilities_json,counts_json,last_probe_at,last_error,metadata_json)
values(?,?,?,?,?,?,?,?,?,?)
on conflict(id) do update set
kind=excluded.kind,label=excluded.label,locator=excluded.locator,health=excluded.health,
capabilities_json=excluded.capabilities_json,counts_json=excluded.counts_json,last_probe_at=excluded.last_probe_at,
last_error=excluded.last_error,metadata_json=excluded.metadata_json`,
		source.ID, source.Kind, source.Label, source.Locator, source.Health, string(caps), string(counts), source.LastProbeAt, source.LastError, string(meta))
	return err
}

func (t *Tideglass) mergePersistedSource(ctx context.Context, source SourceStatus) (SourceStatus, error) {
	var persisted SourceStatus
	var capsJSON, countsJSON, metadataJSON string
	err := t.db.QueryRowContext(ctx, `
select id,kind,label,locator,health,capabilities_json,counts_json,coalesce(last_probe_at,''),last_error,metadata_json
from sources where id = ?`, source.ID).Scan(
		&persisted.ID, &persisted.Kind, &persisted.Label, &persisted.Locator, &persisted.Health,
		&capsJSON, &countsJSON, &persisted.LastProbeAt, &persisted.LastError, &metadataJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return source, nil
	}
	if err != nil {
		return SourceStatus{}, err
	}
	for _, column := range []struct {
		name string
		raw  string
		dst  any
	}{
		{"capabilities_json", capsJSON, &persisted.Capabilities},
		{"counts_json", countsJSON, &persisted.Counts},
		{"metadata_json", metadataJSON, &persisted.Metadata},
	} {
		if err := json.Unmarshal([]byte(column.raw), column.dst); err != nil {
			return SourceStatus{}, fmt.Errorf("decode persisted %s for source %s: %w", column.name, source.ID, err)
		}
	}
	if strings.TrimSpace(source.Kind) == "" {
		source.Kind = persisted.Kind
	}
	if strings.TrimSpace(source.Label) == "" {
		source.Label = persisted.Label
	}
	if persisted.Kind == "import" && strings.TrimSpace(persisted.Locator) != "" {
		source.Locator = persisted.Locator
	} else if strings.TrimSpace(source.Locator) == "" {
		source.Locator = persisted.Locator
	}
	if source.Health == "" || source.Health == "unknown" {
		source.Health = persisted.Health
	}
	if len(source.Capabilities) == 0 {
		source.Capabilities = persisted.Capabilities
	}
	if len(source.Counts) == 0 {
		source.Counts = persisted.Counts
	}
	if strings.TrimSpace(source.LastProbeAt) == "" {
		source.LastProbeAt = persisted.LastProbeAt
	}
	if strings.TrimSpace(source.LastError) == "" {
		source.LastError = persisted.LastError
	}
	source.Metadata = mergeStringMaps(persisted.Metadata, source.Metadata)
	return source, nil
}

func discoverStaticSources() []SourceStatus {
	return []SourceStatus{
		{ID: "gitcrawl", Kind: "crawl", Label: "GitHub", Locator: "~/.config/gitcrawl/gitcrawl.db", Health: "unknown"},
		{ID: "slacrawl", Kind: "crawl", Label: "Slack", Locator: "~/.slacrawl/slacrawl.db", Health: "unknown"},
		{ID: "discrawl", Kind: "crawl", Label: "Discord", Locator: "~/.discrawl/discrawl.db", Health: "unknown"},
		{ID: "notcrawl", Kind: "crawl", Label: "Notion", Locator: "~/.notcrawl/notcrawl.db", Health: "unknown"},
		{ID: "graincrawl", Kind: "crawl", Label: "Granola", Locator: "~/.config/graincrawl/graincrawl.db", Health: "unknown"},
		{ID: "codex", Kind: "import", Label: "Codex sessions", Locator: "~/.codex/sessions", Health: "unknown"},
		{ID: "chatgpt", Kind: "import", Label: "ChatGPT export", Locator: "", Health: "unknown"},
		{ID: "claude", Kind: "import", Label: "Claude export", Locator: "", Health: "unknown"},
	}
}

func discoverCrawlBar(ctx context.Context) ([]SourceStatus, error) {
	bin, err := crawlbarBinary()
	if err != nil {
		return nil, err
	}
	discoveryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(discoveryCtx, bin, "apps", "--json")
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rows []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Available   bool   `json:"available"`
		BinaryPath  string `json:"binary_path"`
		Enabled     bool   `json:"enabled"`
	}
	if err := json.Unmarshal(output, &rows); err != nil {
		return nil, err
	}
	var out []SourceStatus
	for _, row := range rows {
		if !row.Available {
			continue
		}
		out = append(out, SourceStatus{
			ID:       row.ID,
			Kind:     "crawl",
			Label:    row.DisplayName,
			Health:   "available",
			Locator:  crawlbarDatabasePath(ctx, bin, row.ID),
			Metadata: map[string]string{"binary_path": row.BinaryPath, "enabled": fmt.Sprint(row.Enabled)},
		})
	}
	return out, nil
}

func crawlbarBinary() (string, error) {
	for _, envKey := range []string{"TIDEGLASS_CRAWLBAR", "CRAWLBAR_BIN"} {
		if value := strings.TrimSpace(os.Getenv(envKey)); value != "" {
			return value, nil
		}
	}
	if bin, err := exec.LookPath("crawlbar"); err == nil {
		return bin, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		bin := filepath.Join(home, ".local", "bin", "crawlbar")
		if _, statErr := os.Stat(bin); statErr == nil {
			return bin, nil
		}
	}
	return "", errors.New("crawlbar binary not found")
}

func crawlbarDatabasePath(ctx context.Context, bin, appID string) string {
	statusCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(statusCtx, bin, "status", "--app", appID, "--json")
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	var rows []struct {
		DatabasePath string `json:"database_path"`
		Databases    []struct {
			Path      string `json:"path"`
			IsPrimary bool   `json:"is_primary"`
		} `json:"databases"`
	}
	if err := json.Unmarshal(output, &rows); err != nil || len(rows) == 0 {
		return ""
	}
	if rows[0].DatabasePath != "" {
		return rows[0].DatabasePath
	}
	for _, database := range rows[0].Databases {
		if database.IsPrimary && database.Path != "" {
			return database.Path
		}
	}
	if len(rows[0].Databases) > 0 {
		return rows[0].Databases[0].Path
	}
	return ""
}

func probeSource(ctx context.Context, source SourceStatus) SourceStatus {
	source.LastProbeAt = now()
	source.Counts = map[string]int64{}
	source.LastError = ""
	switch source.ID {
	case "gitcrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"threads": "threads", "clusters": "cluster_groups"}, []string{"metadata", "text"})
	case "slacrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"messages": "messages", "channels": "channels", "users": "users", "search": "message_fts"}, []string{"fts", "metadata", "text"})
	case "discrawl":
		out := probeSQLite(ctx, source, source.Locator, map[string]string{"messages": "messages", "members": "members", "embeddings": "message_embeddings", "search": "message_fts"}, []string{"fts", "semantic", "metadata", "text"})
		if out.Health == "ok" && out.Counts["embeddings"] == 0 {
			out.Health = "partial"
			out.LastError = "discrawl has no message embeddings; direct text probe is usable"
		}
		return out
	case "notcrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"pages": "pages", "blocks": "blocks", "comments": "comments", "search": "page_fts"}, []string{"fts", "metadata", "text"})
	case "graincrawl":
		return probeSQLite(ctx, source, source.Locator, map[string]string{"notes": "notes", "transcripts": "transcript_chunks", "search": "notes_fts"}, []string{"fts", "metadata", "text"})
	case "codex":
		count := int64(0)
		err := filepath.WalkDir(expandHome(source.Locator), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
				count++
			}
			return nil
		})
		source.Capabilities = []string{"metadata", "text"}
		source.Counts = map[string]int64{"session_files": count}
		if err != nil {
			source.Health = "error"
			source.LastError = err.Error()
			return source
		}
		source.Health = "ok"
		return source
	default:
		if source.Kind == "import" {
			if source.Health == "" {
				source.Health = "unknown"
			}
			source.Capabilities = []string{"metadata", "text"}
		}
		return source
	}
}

func probeSQLite(ctx context.Context, source SourceStatus, path string, tables map[string]string, caps []string) SourceStatus {
	source.Locator = path
	path = expandHome(path)
	if strings.TrimSpace(path) == "" {
		source.Health = "error"
		source.LastError = "database locator is empty"
		return source
	}
	ro, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		source.Health = "error"
		source.LastError = err.Error()
		return source
	}
	defer ro.Close()
	source.Health = "ok"
	source.Capabilities = caps
	source.Counts = map[string]int64{}
	probeErrors := 0
	for label, table := range tables {
		var count int64
		query := `select count(*) from ` + store.QuoteIdent(table)
		if err := ro.DB().QueryRowContext(ctx, query).Scan(&count); err != nil {
			source.LastError = strings.TrimSpace(source.LastError + "; " + err.Error())
			probeErrors++
			continue
		}
		source.Counts[label] = count
	}
	if probeErrors > 0 {
		source.Health = "partial"
	}
	return source
}

func sourceAllowed(kind, sourceID string) bool {
	switch kind {
	case "social.dinner", "social.dating":
		switch sourceID {
		case "slacrawl", "discrawl", "chatgpt", "claude":
			return true
		default:
			return false
		}
	case "work.project.start", "work.release", "agent.delegation", "work.new_job":
		switch sourceID {
		case "codex", "gitcrawl", "notcrawl", "graincrawl", "slacrawl", "discrawl", "chatgpt", "claude":
			return true
		default:
			return false
		}
	default:
		return true
	}
}
