package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (t *Tideglass) ExportProfile(ctx context.Context, opts ExportOptions) (ExportResult, error) {
	if opts.Format != "" && opts.Format != "tgz" {
		return ExportResult{}, fmt.Errorf("unsupported export format %q", opts.Format)
	}
	profile, err := t.Profile(ctx, ProfileOptions{IntentID: opts.IntentID, Kind: opts.Kind})
	if err != nil {
		return ExportResult{}, err
	}
	exportEvidence := sanitizeEvidenceForExport(profile.Evidence)
	exportProfile := profile
	exportProfile.Evidence = exportEvidence
	out := opts.Out
	if out == "" {
		out = filepath.Join(filepath.Dir(t.path), "tideglass-"+profile.Intent.Kind+"-"+time.Now().UTC().Format("20060102T150405Z")+".tgz")
	} else {
		out = expandHome(out)
	}
	outPath, err := resolvedWritePath(out)
	if err != nil {
		return ExportResult{}, err
	}
	dbPath, err := resolvedWritePath(t.path)
	if err != nil {
		return ExportResult{}, err
	}
	blockedPaths := map[string]bool{
		dbPath:          true,
		dbPath + "-wal": true,
		dbPath + "-shm": true,
	}
	if blockedPaths[outPath] {
		return ExportResult{}, fmt.Errorf("refusing to export over active database path %s", out)
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return ExportResult{}, err
	}
	file, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return ExportResult{}, err
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	records := 0
	if err := writeTarJSON(tw, "manifest.json", map[string]any{"schema_version": "tideglass.bundle.v1", "generated_at": now(), "intent": profile.Intent}); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records++
	if err := writeTarJSON(tw, "profile.json", exportProfile); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records++
	if err := writeTarJSONL(tw, "claims.jsonl", profile.Claims); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records += len(profile.Claims)
	if err := writeTarJSONL(tw, "evidence.jsonl", exportEvidence); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	records += len(profile.Evidence)
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		_ = file.Close()
		return ExportResult{}, err
	}
	if err := gz.Close(); err != nil {
		_ = file.Close()
		return ExportResult{}, err
	}
	if err := file.Close(); err != nil {
		return ExportResult{}, err
	}
	return ExportResult{Path: out, Format: "tgz", Records: records}, nil
}

func sanitizeEvidenceForExport(evidence []EvidenceOut) []EvidenceOut {
	out := make([]EvidenceOut, len(evidence))
	for i, ev := range evidence {
		ev.Locator = sanitizeLocatorJSON(ev.Locator)
		out[i] = ev
	}
	return out
}

func sanitizeLocatorJSON(raw string) string {
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	value = sanitizeLocatorValue("", value)
	data, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(data)
}

func sanitizeLocatorValue(key string, value any) any {
	switch v := value.(type) {
	case map[string]any:
		for childKey, childValue := range v {
			v[childKey] = sanitizeLocatorValue(childKey, childValue)
		}
		return v
	case []any:
		for i, childValue := range v {
			v[i] = sanitizeLocatorValue(key, childValue)
		}
		return v
	case string:
		lowerKey := strings.ToLower(key)
		if filepath.IsAbs(v) && (strings.Contains(lowerKey, "path") || strings.Contains(lowerKey, "file") || strings.Contains(lowerKey, "database")) {
			return filepath.Base(v)
		}
		return v
	default:
		return value
	}
}

func resolvedWritePath(path string) (string, error) {
	abs, err := filepath.Abs(expandHome(path))
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent := filepath.Dir(abs)
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Clean(filepath.Join(resolvedParent, filepath.Base(abs))), nil
	}
	return filepath.Clean(abs), nil
}

func writeTarJSON(tw *tar.Writer, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

func writeTarJSONL[T any](tw *tar.Writer, name string, rows []T) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			return err
		}
	}
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(buf.Len()), ModTime: time.Now()}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	_, err := tw.Write(buf.Bytes())
	return err
}
