package app

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func defaultDBPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return expandHome(path), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".tideglass", "tideglass.db"), nil
}

func normalizeKind(kind, query string) string {
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind != "" {
		return kind
	}
	q := strings.ToLower(query)
	switch {
	case strings.Contains(q, "dinner"):
		return "social.dinner"
	case strings.Contains(q, "new job"):
		return "work.new_job"
	case strings.Contains(q, "dating") || hasDatingPhrase(q):
		return "social.dating"
	case strings.Contains(q, "agent"):
		return "agent.delegation"
	default:
		return "work.project.start"
	}
}

func rawTokenSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, token := range wordRE.FindAllString(strings.ToLower(text), -1) {
		token = strings.Trim(token, "_")
		if token != "" {
			out[token] = true
		}
	}
	return out
}

func hasDatingPhrase(text string) bool {
	tokens := wordTokens(text)
	return hasTokenSequence(tokens, "first", "dates") ||
		hasTokenSequence(tokens, "first", "date") ||
		hasTokenSequence(tokens, "better", "dates") ||
		hasTokenSequence(tokens, "better", "date") ||
		hasTokenSequence(tokens, "plan", "date") ||
		hasTokenSequence(tokens, "date", "ideas") ||
		hasTokenSequence(tokens, "date", "someone") ||
		hasTokenSequence(tokens, "dating", "ideas")
}

func hasTokenSequence(tokens []string, sequence ...string) bool {
	if len(sequence) == 0 || len(sequence) > len(tokens) {
		return false
	}
	for i := 0; i <= len(tokens)-len(sequence); i++ {
		matched := true
		for j, token := range sequence {
			if tokens[i+j] != token {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func titleForKind(kind string) string {
	switch kind {
	case "social.dinner":
		return "Dinner with strangers"
	case "social.dating":
		return "Dating preferences"
	case "work.new_job":
		return "New job onboarding"
	case "agent.delegation":
		return "Agent delegation"
	default:
		return "Project start"
	}
}

func now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func id(prefix string) string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf[:])
}

func hashText(text string) string {
	return hashBytes([]byte(text))
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeText(text string) string {
	return strings.Join(words(text), " ")
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func mergeStringMaps(left, right map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range left {
		out[k] = v
	}
	for k, v := range right {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
