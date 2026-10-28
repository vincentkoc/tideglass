package app

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func renderAgentContext(intent IntentOut, claims []ClaimOut, unresolved []string, budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "intent: %s (%s)\n", intent.Title, intent.Kind)
	if len(claims) > 0 {
		b.WriteString("\npreferences and constraints:\n")
		for _, claim := range claims {
			fmt.Fprintf(&b, "- %s: %s (confidence %.2f)\n", claim.Kind, terminalSafeInline(claim.Value), claim.Confidence)
		}
	}
	if len(unresolved) > 0 {
		b.WriteString("\nunresolved:\n")
		for _, question := range unresolved {
			fmt.Fprintf(&b, "- %s\n", question)
		}
	}
	text := strings.TrimSpace(b.String())
	if budget > 0 && len(text) > budget {
		return strings.TrimSpace(text[:budget]) + "\n..."
	}
	return text
}

func Print(value any, jsonOut bool) error {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	switch v := value.(type) {
	case SourceList:
		for _, source := range v.Sources {
			fmt.Printf("%-10s %-8s %-7s %s\n", terminalSafeInline(source.ID), terminalSafeInline(source.Kind), terminalSafeInline(source.Health), terminalSafeInline(source.Locator))
			if source.LastError != "" {
				fmt.Printf("  warning: %s\n", terminalSafeInline(source.LastError))
			}
		}
	case IngestResult:
		fmt.Printf("ingested %s: artifacts=%d evidence=%d claims=%d\n", v.Kind, v.Artifacts, v.Evidence, v.Claims)
	case AskResult:
		fmt.Printf("intent: %s (%s)\n", v.Intent.Title, v.Intent.Kind)
		fmt.Println("claims:")
		for _, claim := range v.Claims {
			fmt.Printf("- [%s] %s (%.2f)\n", claim.Kind, terminalSafeInline(claim.Value), claim.Confidence)
		}
		if len(v.UnresolvedQuestions) > 0 {
			fmt.Println("unresolved:")
			for _, question := range v.UnresolvedQuestions {
				fmt.Printf("- %s\n", question)
			}
		}
		fmt.Println("source coverage:")
		for _, coverage := range v.SourceCoverage {
			fmt.Printf("- %s: %s hits=%d", coverage.SourceID, coverage.Health, coverage.Hits)
			if coverage.Error != "" {
				fmt.Printf(" warning=%s", terminalSafeInline(coverage.Error))
			}
			fmt.Println()
		}
	case ProfileResult:
		if v.Text != "" {
			fmt.Println(v.Text)
			return nil
		}
		fmt.Printf("intent: %s (%s)\n", v.Intent.Title, v.Intent.Kind)
		for _, claim := range v.Claims {
			fmt.Printf("- [%s] %s (%.2f)\n", claim.Kind, terminalSafeInline(claim.Value), claim.Confidence)
		}
	case EditResult:
		fmt.Printf("edited %s -> %s\n", v.ClaimID, terminalSafeInline(v.Value))
	case ExportResult:
		fmt.Printf("exported %s records=%d\n", terminalSafeInline(v.Path), v.Records)
	case DoctorResult:
		fmt.Printf("doctor: %s db=%s schema=%d\n", terminalSafeInline(v.OverallState), terminalSafeInline(v.DBPath), v.Schema)
		for _, check := range v.Checks {
			fmt.Printf("- %s: %s %s\n", check.ID, check.Status, terminalSafeInline(check.Message))
		}
	case []EvidenceOut:
		for _, ev := range v {
			fmt.Printf("- %s %s %s\n", ev.SourceID, ev.ID, terminalSafeInline(ev.Snippet))
		}
	default:
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	return nil
}

func terminalSafeInline(text string) string {
	text = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		case '\x1b', '\x7f':
			return -1
		}
		if r < 0x20 || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

func PrintText(format string, args ...any) error {
	_, err := fmt.Fprintf(os.Stdout, format, args...)
	return err
}
