package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincentkoc/tideglass/internal/app"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tideglass:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	command := args[0]
	args = args[1:]
	switch command {
	case "init":
		return runInit(ctx, args)
	case "sources":
		return runSources(ctx, args)
	case "ingest":
		return runIngest(ctx, args)
	case "ask":
		return runAsk(ctx, args)
	case "profile":
		return runProfile(ctx, args)
	case "evidence":
		return runEvidence(ctx, args)
	case "doctor":
		return runDoctor(ctx, args)
	case "context":
		return runContext(ctx, args)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	return app.PrintText("initialized %s\n", tg.Path())
}

func runSources(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sources", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "write JSON")
	probe := fs.Bool("probe", false, "probe local source schemas")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	out, err := tg.Sources(ctx, app.SourceOptions{Probe: *probe})
	if err != nil {
		return err
	}
	return app.Print(out, *jsonOut)
}

func runIngest(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("ingest requires a kind: chatgpt, claude, or codex")
	}
	kind := args[0]
	fs := flag.NewFlagSet("ingest "+kind, flag.ContinueOnError)
	path := fs.String("path", "", "input path")
	limit := fs.Int("limit", 0, "maximum artifacts to import")
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args[1:])); err != nil {
		return err
	}
	if strings.TrimSpace(*path) == "" {
		return errors.New("ingest --path is required")
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.Ingest(ctx, app.IngestOptions{Kind: kind, Path: expandPath(*path), Limit: *limit})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	kind := fs.String("kind", "", "intent kind")
	jsonOut := fs.Bool("json", false, "write JSON")
	explain := fs.Bool("explain", false, "include evidence detail")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return errors.New("ask requires a query")
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.Ask(ctx, app.AskOptions{Kind: *kind, Query: query, Explain: *explain})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runProfile(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("profile requires a subcommand: show, edit, or export")
	}
	switch args[0] {
	case "show":
		return runProfileShow(ctx, args[1:])
	case "edit":
		return runProfileEdit(ctx, args[1:])
	case "export":
		return runProfileExport(ctx, args[1:])
	default:
		return fmt.Errorf("unknown profile subcommand %q", args[0])
	}
}

func runProfileShow(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("profile show", flag.ContinueOnError)
	intentID := fs.String("intent", "", "intent ID")
	kind := fs.String("kind", "", "intent kind")
	forAgent := fs.String("for-agent", "", "agent target")
	budget := fs.Int("budget", 0, "approximate character budget")
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.Profile(ctx, app.ProfileOptions{IntentID: *intentID, Kind: *kind, ForAgent: *forAgent, Budget: *budget})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runProfileEdit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("profile edit", flag.ContinueOnError)
	value := fs.String("set", "", "replacement value")
	reason := fs.String("reason", "", "edit reason")
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("profile edit requires a claim id")
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.EditClaim(ctx, app.EditOptions{ClaimID: fs.Arg(0), Value: *value, Reason: *reason})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runProfileExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("profile export", flag.ContinueOnError)
	intentID := fs.String("intent", "", "intent ID")
	kind := fs.String("kind", "", "intent kind")
	format := fs.String("format", "tgz", "export format")
	out := fs.String("out", "", "output path")
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.ExportProfile(ctx, app.ExportOptions{IntentID: *intentID, Kind: *kind, Format: *format, Out: *out})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runEvidence(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "show" {
		return errors.New("evidence requires: show <claim-id>")
	}
	fs := flag.NewFlagSet("evidence show", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args[1:])); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("evidence show requires a claim id")
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.Evidence(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.Doctor(ctx)
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runContext(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	intentID := fs.String("intent", "", "intent ID")
	kind := fs.String("kind", "", "intent kind")
	forAgent := fs.String("for-agent", "codex", "agent target")
	budget := fs.Int("budget", 1200, "approximate character budget")
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.Profile(ctx, app.ProfileOptions{IntentID: *intentID, Kind: *kind, ForAgent: *forAgent, Budget: *budget})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: tideglass <command> [options]

commands:
  init
  sources [--probe] [--json]
  ingest chatgpt|claude|codex --path <path> [--limit n]
  ask [--kind kind] [--json] <query>
  profile show --kind <kind>|--intent <id>
  profile edit <claim-id> --set <value>
  profile export --kind <kind>|--intent <id>
  evidence show <claim-id>
  context --kind <kind> --for-agent codex
  doctor
`)
}

func normalizeFlagArgs(args []string) []string {
	valueFlags := map[string]bool{
		"budget":    true,
		"db":        true,
		"for-agent": true,
		"format":    true,
		"intent":    true,
		"kind":      true,
		"limit":     true,
		"out":       true,
		"path":      true,
		"reason":    true,
		"set":       true,
	}
	var flags []string
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		flags = append(flags, arg)
		name := strings.TrimLeft(arg, "-")
		if cut, _, ok := strings.Cut(name, "="); ok {
			name = cut
		}
		if valueFlags[name] && !strings.Contains(arg, "=") && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positionals...)
}

func expandPath(path string) string {
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
