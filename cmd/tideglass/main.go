package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincentkoc/tideglass/internal/app"
)

var version = "dev"

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
	if command == "--version" || command == "-v" {
		fmt.Println(version)
		return nil
	}
	args = args[1:]
	switch command {
	case "version":
		fmt.Println(version)
		return nil
	case "init":
		return runInit(ctx, args)
	case "sources":
		return runSources(ctx, args)
	case "ingest":
		return runIngest(ctx, args)
	case "ask":
		return runAsk(ctx, args)
	case "review":
		return runReview(ctx, args)
	case "profile":
		return runProfile(ctx, args)
	case "evidence":
		return runEvidence(ctx, args)
	case "doctor":
		return runDoctor(ctx, args)
	case "context":
		return runContext(ctx, args)
	case "resolve":
		return runResolve(ctx, args)
	case "serve":
		return runServe(ctx, args)
	case "mcp":
		return runMCP(ctx, args)
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

func runReview(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	intentID := fs.String("intent", "", "intent ID")
	kind := fs.String("kind", "", "intent kind")
	all := fs.Bool("all", false, "include already accepted claims")
	jsonOut := fs.Bool("json", false, "write review candidates as JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	profile, err := tg.Profile(ctx, app.ProfileOptions{IntentID: *intentID, Kind: *kind})
	if err != nil {
		return err
	}
	if *jsonOut {
		return app.Print(profile, true)
	}
	reader := bufio.NewReader(os.Stdin)
	reviewed := 0
	for _, claim := range profile.Claims {
		if claim.Status == "accepted" && !*all {
			continue
		}
		fmt.Printf("\n%s\n", strings.Repeat("─", 72))
		fmt.Printf("claim: %s\nkind: %s\nstatus: %s confidence: %.2f\n", claim.ID, claim.Kind, claim.Status, claim.Confidence)
		fmt.Printf("value: %s\n", safeLine(claim.Value))
		if len(claim.Evidence) > 0 {
			fmt.Printf("evidence: %s\n", strings.Join(claim.Evidence, ", "))
		}
		fmt.Print("[a]ccept [e]dit+accept [r]eject [s]kip [q]uit > ")
		answer, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(strings.ToLower(answer)) {
		case "a", "accept":
			if _, err := tg.ReviewClaim(ctx, app.ReviewOptions{ClaimID: claim.ID, Action: "accept", Reason: "interactive review"}); err != nil {
				return err
			}
			reviewed++
			fmt.Println("accepted")
		case "e", "edit":
			fmt.Print("new value > ")
			value, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			value = strings.TrimSpace(value)
			if value == "" {
				fmt.Println("skipped empty edit")
				continue
			}
			if _, err := tg.EditClaim(ctx, app.EditOptions{ClaimID: claim.ID, Value: value, Reason: "interactive review edit"}); err != nil {
				return err
			}
			if _, err := tg.ReviewClaim(ctx, app.ReviewOptions{ClaimID: claim.ID, Action: "accept", Reason: "interactive review edit"}); err != nil {
				return err
			}
			reviewed++
			fmt.Println("edited and accepted")
		case "r", "reject":
			if _, err := tg.ReviewClaim(ctx, app.ReviewOptions{ClaimID: claim.ID, Action: "reject", Reason: "interactive review"}); err != nil {
				return err
			}
			reviewed++
			fmt.Println("rejected")
		case "q", "quit":
			fmt.Printf("reviewed %d claims\n", reviewed)
			return nil
		default:
			fmt.Println("skipped")
		}
	}
	fmt.Printf("reviewed %d claims\n", reviewed)
	return nil
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

func runResolve(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	requestPath := fs.String("request", "", "request envelope JSON path")
	jsonOut := fs.Bool("json", false, "write JSON")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	request := app.IntentRequestEnvelope{}
	if strings.TrimSpace(*requestPath) != "" {
		data, err := os.ReadFile(expandPath(*requestPath))
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &request); err != nil {
			return err
		}
	} else {
		if fs.NArg() != 1 {
			return errors.New("resolve requires a tideglass:// URI or --request")
		}
		request.URI = fs.Arg(0)
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	result, err := tg.ResolveIntent(ctx, app.ResolveOptions{Request: request})
	if err != nil {
		return err
	}
	return app.Print(result, *jsonOut)
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	server := &http.Server{Addr: *addr, Handler: app.NewServiceHandler(tg)}
	fmt.Fprintf(os.Stderr, "tideglass: serving on http://%s\n", *addr)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func runMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	once := fs.Bool("once", false, "handle one JSON-RPC request on stdin")
	dbPath := fs.String("db", "", "database path")
	if err := fs.Parse(normalizeFlagArgs(args)); err != nil {
		return err
	}
	if !*once {
		return errors.New("mcp currently requires --once")
	}
	tg, err := app.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer tg.Close()
	return app.HandleMCPOnce(ctx, tg, os.Stdin, os.Stdout)
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: tideglass <command> [options]

commands:
  init
  sources [--probe] [--json]
	  ingest chatgpt|claude|codex --path <path> [--limit n]
	  ask [--kind kind] [--json] <query>
	  review --kind <kind>|--intent <id>
	  profile show --kind <kind>|--intent <id>
  profile edit <claim-id> --set <value>
  profile export --kind <kind>|--intent <id>
  evidence show <claim-id>
	  context --kind <kind> --for-agent codex
	  resolve tideglass://intent/<kind> [--json]
	  serve [--addr 127.0.0.1:8765]
	  mcp --once
	  doctor
	  version
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
		"request":   true,
		"set":       true,
		"addr":      true,
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

func safeLine(text string) string {
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
