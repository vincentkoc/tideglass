package app

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const assistantJSONLimit = 512 * 1024 * 1024

func (t *Tideglass) Ingest(ctx context.Context, opts IngestOptions) (IngestResult, error) {
	kind := strings.TrimSpace(strings.ToLower(opts.Kind))
	if kind == "" {
		return IngestResult{}, errors.New("ingest kind is required")
	}
	var artifacts []artifact
	var claims []candidateClaim
	var err error
	switch kind {
	case "codex":
		artifacts, claims, err = importCodex(opts.Path, opts.Limit)
	case "chatgpt":
		artifacts, claims, err = importAssistantExport("chatgpt", opts.Path, opts.Limit)
	case "claude":
		artifacts, claims, err = importAssistantExport("claude", opts.Path, opts.Limit)
	default:
		return IngestResult{}, fmt.Errorf("unsupported ingest kind %q", opts.Kind)
	}
	if err != nil {
		return IngestResult{}, err
	}
	source := SourceStatus{
		ID:           kind,
		Kind:         "import",
		Label:        kind,
		Locator:      opts.Path,
		Health:       "ok",
		Capabilities: []string{"metadata", "text"},
		LastProbeAt:  now(),
	}
	if err := t.upsertSource(ctx, source); err != nil {
		return IngestResult{}, err
	}
	var evidenceCount int
	importedClaims := 0
	for _, art := range artifacts {
		if art.SourceID == "" {
			art.SourceID = kind
		}
		evidenceID, err := t.upsertArtifactEvidence(ctx, art)
		if err != nil {
			return IngestResult{}, err
		}
		evidenceCount++
		if err := t.embedText(ctx, "evidence", evidenceID, art.Snippet); err != nil {
			return IngestResult{}, err
		}
		if (kind == "chatgpt" || kind == "claude") && looksLikeMemory(art.Title, art.Snippet) {
			existing, err := t.claimExistsForEvidence(ctx, evidenceID, "preference.agent.imported_memory")
			if err != nil {
				return IngestResult{}, err
			}
			if existing {
				continue
			}
			intent, err := t.ensureIntent(ctx, "agent.delegation", "Agent delegation")
			if err != nil {
				return IngestResult{}, err
			}
			claimID, err := t.insertClaim(ctx, intent.ID, candidateClaim{
				Kind:       "preference.agent.imported_memory",
				Value:      art.Snippet,
				Confidence: 0.9,
				SourceMode: "imported_memory",
				EvidenceID: evidenceID,
			})
			if err != nil {
				return IngestResult{}, err
			}
			if err := t.linkClaimEvidence(ctx, claimID, evidenceID, "supporting"); err != nil {
				return IngestResult{}, err
			}
			importedClaims++
		}
	}
	for _, claim := range claims {
		intent, err := t.ensureIntent(ctx, "agent.delegation", "Agent delegation")
		if err != nil {
			return IngestResult{}, err
		}
		claimID, err := t.insertClaim(ctx, intent.ID, claim)
		if err != nil {
			return IngestResult{}, err
		}
		if claim.EvidenceID != "" {
			if err := t.linkClaimEvidence(ctx, claimID, claim.EvidenceID, "supporting"); err != nil {
				return IngestResult{}, err
			}
		}
		importedClaims++
	}
	return IngestResult{Kind: kind, Path: opts.Path, Artifacts: len(artifacts), Evidence: evidenceCount, Claims: importedClaims}, nil
}

func (t *Tideglass) claimExistsForEvidence(ctx context.Context, evidenceID, kind string) (bool, error) {
	var existing string
	err := t.db.QueryRowContext(ctx, `
select c.id
from claims c
join claim_evidence ce on ce.claim_id = c.id
where ce.evidence_id = ? and c.kind = ?
limit 1`, evidenceID, kind).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (t *Tideglass) upsertArtifactEvidence(ctx context.Context, art artifact) (string, error) {
	if art.ContentHash == "" {
		art.ContentHash = hashText(art.Snippet)
	}
	if art.MetadataJSON == "" {
		art.MetadataJSON = "{}"
	}
	artifactID := "art_" + hashText(art.SourceID + "|" + art.ExternalID)[:24]
	_, err := t.db.ExecContext(ctx, `
insert into source_artifacts(id,source_id,external_id,artifact_kind,title,author_ref,created_at,updated_at,content_hash,metadata_json)
values(?,?,?,?,?,?,?,?,?,?)
on conflict(source_id, external_id) do update set
artifact_kind=excluded.artifact_kind,title=excluded.title,author_ref=excluded.author_ref,
created_at=excluded.created_at,updated_at=excluded.updated_at,content_hash=excluded.content_hash,metadata_json=excluded.metadata_json`,
		artifactID, art.SourceID, art.ExternalID, art.Kind, trimForSnippet(art.Title, 300), art.AuthorRef, art.CreatedAt, art.UpdatedAt, art.ContentHash, art.MetadataJSON)
	if err != nil {
		return "", err
	}
	evidenceID := "ev_" + hashText(artifactID + "|" + art.LocatorJSON + "|" + art.Snippet)[:24]
	if art.LocatorJSON == "" {
		locator, _ := json.Marshal(map[string]string{"external_id": art.ExternalID})
		art.LocatorJSON = string(locator)
	}
	_, err = t.db.ExecContext(ctx, `
insert into evidence(id,source_artifact_id,locator_json,snippet,snippet_hash,observed_at)
values(?,?,?,?,?,?)
on conflict(id) do update set snippet=excluded.snippet,snippet_hash=excluded.snippet_hash,observed_at=excluded.observed_at`,
		evidenceID, artifactID, art.LocatorJSON, trimForSnippet(art.Snippet, 1000), hashText(art.Snippet), now())
	if err != nil {
		return "", err
	}
	_, err = t.db.ExecContext(ctx, `insert or replace into evidence_fts(rowid,evidence_id,source_id,snippet) select rowid, ?, ?, ? from evidence where id = ?`,
		evidenceID, art.SourceID, trimForSnippet(art.Snippet, 1000), evidenceID)
	return evidenceID, err
}

func importCodex(root string, limit int) ([]artifact, []candidateClaim, error) {
	root = expandHome(root)
	type codexFile struct {
		Path    string
		ModTime time.Time
	}
	var files []codexFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return statErr
		}
		files = append(files, codexFile{Path: path, ModTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].ModTime.After(files[j].ModTime)
	})
	var artifacts []artifact
	for _, fileInfo := range files {
		path := fileInfo.Path
		if limit > 0 && len(artifacts) >= limit {
			break
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		reader := bufio.NewReaderSize(file, 1024*1024)
		lineNo := 0
		cwd := ""
		for {
			if limit > 0 && len(artifacts) >= limit {
				break
			}
			line, readErr := reader.ReadBytes('\n')
			if len(line) == 0 && readErr == io.EOF {
				break
			}
			if readErr != nil && readErr != io.EOF {
				_ = file.Close()
				return nil, nil, readErr
			}
			lineNo++
			if bytes.Contains(line, []byte(`"function_call_output"`)) ||
				bytes.Contains(line, []byte(`"tool_output"`)) {
				if readErr == io.EOF {
					break
				}
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal(line, &obj); err != nil {
				if readErr == io.EOF {
					break
				}
				continue
			}
			if typ, _ := obj["type"].(string); typ == "session_meta" {
				if payload, _ := obj["payload"].(map[string]any); payload != nil {
					cwd, _ = payload["cwd"].(string)
				}
				if readErr == io.EOF {
					break
				}
				continue
			}
			text := codexEventText(obj)
			if len(text) < 40 {
				if readErr == io.EOF {
					break
				}
				continue
			}
			meta, _ := json.Marshal(map[string]any{"path": path, "line": lineNo, "cwd": cwd})
			locator, _ := json.Marshal(map[string]any{"path": path, "line": lineNo})
			title := filepath.Base(path)
			if cwd != "" {
				title = cwd
			}
			artifacts = append(artifacts, artifact{
				SourceID:     "codex",
				ExternalID:   fmt.Sprintf("%s:%d", path, lineNo),
				Kind:         "codex_event",
				Title:        title,
				ContentHash:  hashBytes(line),
				MetadataJSON: string(meta),
				Snippet:      trimForSnippet(text, 1000),
				LocatorJSON:  string(locator),
			})
			if readErr == io.EOF {
				break
			}
		}
		_ = file.Close()
	}
	return artifacts, nil, nil
}

func importAssistantExport(kind, path string, limit int) ([]artifact, []candidateClaim, error) {
	path = expandHome(path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	var artifacts []artifact
	if info.IsDir() {
		artifacts, err = importAssistantDir(kind, path, limit)
	} else if strings.HasSuffix(strings.ToLower(path), ".zip") {
		artifacts, err = importAssistantZip(kind, path, limit)
	} else {
		data, readErr := readFileLimited(path)
		err = readErr
		if err == nil {
			artifacts, err = appendAssistantArtifacts(kind, nil, namedBlob{Name: filepath.Base(path), Data: data}, limit)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	var claims []candidateClaim
	return artifacts, claims, nil
}

type namedBlob struct {
	Name string
	Data []byte
}

func importAssistantZip(kind, path string, limit int) ([]artifact, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var artifacts []artifact
	for _, file := range zr.File {
		if limit > 0 && len(artifacts) >= limit {
			break
		}
		if file.FileInfo().IsDir() || !strings.HasSuffix(strings.ToLower(file.Name), ".json") {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, err := readAllLimited(rc, assistantJSONLimit, file.Name)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		artifacts, err = appendAssistantArtifacts(kind, artifacts, namedBlob{Name: file.Name, Data: data}, limit)
		if err != nil {
			return nil, err
		}
	}
	return artifacts, nil
}

func importAssistantDir(kind, root string, limit int) ([]artifact, error) {
	var artifacts []artifact
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if limit > 0 && len(artifacts) >= limit {
			return filepath.SkipAll
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".json") {
			return nil
		}
		data, err := readFileLimited(path)
		if err != nil {
			return err
		}
		artifacts, err = appendAssistantArtifacts(kind, artifacts, namedBlob{Name: path, Data: data}, limit)
		return err
	})
	return artifacts, err
}

func appendAssistantArtifacts(kind string, artifacts []artifact, blob namedBlob, limit int) ([]artifact, error) {
	var value any
	if err := json.Unmarshal(blob.Data, &value); err != nil {
		return artifacts, fmt.Errorf("parse %s: %w", blob.Name, err)
	}
	texts := collectAssistantTexts(value)
	for index, text := range texts {
		if limit > 0 && len(artifacts) >= limit {
			break
		}
		text = trimForSnippet(text, 1000)
		if len(text) < 40 {
			continue
		}
		locator, _ := json.Marshal(map[string]any{"file": blob.Name, "index": index})
		artifacts = append(artifacts, artifact{
			SourceID:     kind,
			ExternalID:   fmt.Sprintf("%s:%d", blob.Name, index),
			Kind:         "assistant_export_text",
			Title:        blob.Name,
			ContentHash:  hashText(text),
			MetadataJSON: "{}",
			Snippet:      text,
			LocatorJSON:  string(locator),
		})
	}
	return artifacts, nil
}

func readFileLimited(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readAllLimited(file, assistantJSONLimit, path)
}

func readAllLimited(reader io.Reader, limit int64, name string) ([]byte, error) {
	var buf bytes.Buffer
	written, err := io.Copy(&buf, io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if written > limit {
		return nil, fmt.Errorf("%s is too large to import safely: %d bytes exceeds %d byte limit", name, written, limit)
	}
	return buf.Bytes(), nil
}

func codexEventText(obj map[string]any) string {
	typ, _ := obj["type"].(string)
	payload, _ := obj["payload"].(map[string]any)
	if payload == nil {
		return ""
	}
	var text string
	switch typ {
	case "event_msg":
		msgType, _ := payload["type"].(string)
		if msgType != "user_message" && msgType != "agent_message" {
			return ""
		}
		text, _ = payload["message"].(string)
	case "response_item":
		itemType, _ := payload["type"].(string)
		if itemType != "message" {
			return ""
		}
		role, _ := payload["role"].(string)
		if role != "user" && role != "assistant" {
			return ""
		}
		text = strings.Join(collectTexts(payload["content"], nil), "\n")
	default:
		return ""
	}
	text = trimForSnippet(text, 1000)
	lower := strings.ToLower(text)
	if strings.Contains(lower, "agеnts.md instructions") ||
		strings.Contains(lower, "agents.md instructions") ||
		strings.Contains(lower, "codex_internal_context") ||
		strings.Contains(lower, "base_instructions") ||
		strings.Contains(lower, "<instructions>") {
		return ""
	}
	if looksLikePathOnly(text) {
		return ""
	}
	return text
}

func collectTexts(value any, out []string) []string {
	switch v := value.(type) {
	case string:
		if usefulString(v) {
			out = append(out, v)
		}
	case []any:
		for _, item := range v {
			out = collectTexts(item, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			item := v[key]
			lower := strings.ToLower(key)
			if noisyExportKey(lower) {
				continue
			}
			if lower == "content" || lower == "text" || lower == "message" || lower == "memory" || lower == "title" || lower == "summary" || lower == "name" {
				out = collectTexts(item, out)
				continue
			}
			if lower == "payload" || lower == "items" || lower == "parts" || lower == "messages" || lower == "chat_messages" || lower == "mapping" || lower == "children" {
				out = collectTexts(item, out)
				continue
			}
			switch item.(type) {
			case map[string]any, []any:
				out = collectTexts(item, out)
			}
		}
	}
	return out
}

func collectAssistantTexts(value any) []string {
	return collectAssistantTextsValue(value, nil)
}

func collectAssistantTextsValue(value any, out []string) []string {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			out = collectAssistantTextsValue(item, out)
		}
	case map[string]any:
		if role, ok := assistantMessageRole(v); ok {
			if !userAssistantExportRole(role) {
				return out
			}
			out = collectAssistantMessageText(v, out)
			if memory, ok := lookupMapValue(v, "memory"); ok {
				out = collectTexts(memory, out)
			}
			return out
		}
		if memory, ok := lookupMapValue(v, "memory"); ok {
			out = collectTexts(memory, out)
		}
		if message, ok := lookupMapValue(v, "message"); ok {
			out = collectAssistantTextsValue(message, out)
		}
		for _, key := range []string{"messages", "chat_messages", "mapping", "conversations", "items", "children", "payload"} {
			if item, ok := lookupMapValue(v, key); ok {
				if key == "mapping" {
					if mapping, ok := item.(map[string]any); ok {
						keys := make([]string, 0, len(mapping))
						for mappingKey := range mapping {
							keys = append(keys, mappingKey)
						}
						sort.Strings(keys)
						for _, mappingKey := range keys {
							out = collectAssistantTextsValue(mapping[mappingKey], out)
						}
						continue
					}
				}
				out = collectAssistantTextsValue(item, out)
			}
		}
	}
	return out
}

func assistantMessageRole(v map[string]any) (string, bool) {
	for _, key := range []string{"role", "sender"} {
		if item, ok := lookupMapValue(v, key); ok {
			if role, ok := item.(string); ok && strings.TrimSpace(role) != "" {
				return role, true
			}
		}
	}
	if author, ok := lookupMapValue(v, "author"); ok {
		if authorMap, ok := author.(map[string]any); ok {
			if item, ok := lookupMapValue(authorMap, "role"); ok {
				if role, ok := item.(string); ok && strings.TrimSpace(role) != "" {
					return role, true
				}
			}
		}
	}
	return "", false
}

func userAssistantExportRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user", "human":
		return true
	default:
		return false
	}
}

func collectAssistantMessageText(v map[string]any, out []string) []string {
	for _, key := range []string{"content", "text", "parts"} {
		if item, ok := lookupMapValue(v, key); ok {
			out = collectTexts(item, out)
		}
	}
	return out
}

func lookupMapValue(v map[string]any, key string) (any, bool) {
	for candidate, value := range v {
		if strings.EqualFold(candidate, key) {
			return value, true
		}
	}
	return nil, false
}

func noisyExportKey(key string) bool {
	switch key {
	case "id", "uuid", "conversation_id", "parent", "parent_id", "children_ids", "create_time", "update_time", "timestamp", "metadata", "author", "recipient", "status", "end_turn", "weight", "model", "finish_details":
		return true
	default:
		return strings.HasSuffix(key, "_id") || strings.HasSuffix(key, "_ids")
	}
}

func usefulString(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) < 20 {
		return false
	}
	letters := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return letters >= 12
}

func looksLikePathOnly(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "/Users/") && !strings.Contains(text, " ")
}

func looksLikeMemory(name, text string) bool {
	lower := strings.ToLower(name + " " + text)
	return strings.Contains(lower, "memory") || strings.Contains(lower, "preference") || strings.Contains(lower, "remember")
}
