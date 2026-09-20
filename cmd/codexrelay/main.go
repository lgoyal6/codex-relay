// Command codexrelay is the single local service: proxy, dashboard, and CLI in one
// executable.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/lgoyal6/codex-relay/internal/clock"
	"github.com/lgoyal6/codex-relay/internal/httpapi"
	"github.com/lgoyal6/codex-relay/internal/integration"
	"github.com/lgoyal6/codex-relay/internal/paths"
	"github.com/lgoyal6/codex-relay/internal/policy"
	"github.com/lgoyal6/codex-relay/internal/proxy"
	"github.com/lgoyal6/codex-relay/internal/routing"
	"github.com/lgoyal6/codex-relay/internal/secrets"
	"github.com/lgoyal6/codex-relay/internal/service"
	"github.com/lgoyal6/codex-relay/internal/store"
)

// Version is stamped at build time with -ldflags.
var Version = "dev"

const defaultUpstream = "https://chatgpt.com/backend-api"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "status":
		err = cmdStatus(args)
	case "setup":
		err = cmdSetup(args)
	case "rollback":
		err = cmdRollback(args)
	case "migrate":
		err = cmdMigrate(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "rules":
		err = cmdRules(args)
	case "profile", "relaypool":
		err = cmdProfile(args)
	case "doctor":
		err = cmdDoctor(args)
	case "version", "--version", "-v":
		fmt.Println("codexrelay", Version)
	case "help", "--help", "-h":
		usage()
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`codexrelay - route Codex through your own ChatGPT workspaces with quota rules

Usage:
  codexrelay serve      Run the local service and dashboard
  codexrelay setup      Detect Codex, preview the config change, and apply it
  codexrelay status     Show which workspace a new conversation would use, and why
  codexrelay rules      List the current rules
  codexrelay profile    List, activate, and inspect routing profiles
  codexrelay migrate    Move over from another Codex router, explaining what you must redo
  codexrelay rollback   Undo the Codex configuration change
  codexrelay uninstall  Explain and undo everything codex-relay set up
  codexrelay doctor     Print a redacted diagnostic report
  codexrelay version

Every dashboard action has a CLI equivalent; neither is required to use the other.
`)
}

type app struct {
	svc   *service.Service
	conn  *service.Connector
	close func()
}

func boot(logLevel slog.Level) (*app, error) {
	dbPath, err := paths.DatabasePath()
	if err != nil {
		return nil, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	clk := clock.System()
	// Health probing is cached: it is a real round trip against the platform store, and the
	// dashboard reads state on a poll. Credential reads and writes still go straight through.
	sec := secrets.NewCached(secrets.NewOS())
	reg := routing.NewRegistry(clk)
	creds := service.NewCredentialManager(sec, clk, "")
	svc := service.New(db, reg, creds, sec, clk, log)
	svc.UpstreamBase = upstreamBase()
	svc.Start()
	if err := svc.Refresh(context.Background()); err != nil {
		return nil, err
	}
	conn := service.NewConnector(svc, "", upstreamBase())
	return &app{svc: svc, conn: conn, close: func() { svc.Close(); _ = db.Close() }}, nil
}

func upstreamBase() string {
	if v := strings.TrimSpace(os.Getenv("CODEXRELAY_UPSTREAM")); v != "" {
		return v
	}
	return defaultUpstream
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7788", "loopback address for the proxy and dashboard")
	openBrowser := fs.Bool("open", false, "open the dashboard in your browser")
	verbose := fs.Bool("verbose", false, "verbose logging")
	_ = fs.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	a, err := boot(level)
	if err != nil {
		return err
	}
	defer a.close()

	// Quota is otherwise only learned from turns this process forwards, so an idle or paused
	// workspace would keep a reading from whenever it last served one.
	pollCtx, stopPolling := context.WithCancel(context.Background())
	defer stopPolling()
	a.svc.StartUsagePolling(pollCtx)
	// Schedules are evaluated on the same lifetime as polling: both are background work that
	// belongs only to a long-running serve.
	a.svc.StartAutomations(pollCtx)

	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("invalid --addr: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		// Remote access is out of scope for this release and is refused rather than
		// quietly allowed.
		return fmt.Errorf("codexrelay binds to loopback only; %q is not a loopback address", host)
	}

	guard, err := httpapi.NewGuard(port)
	if err != nil {
		return err
	}
	api := &httpapi.API{
		Svc: a.svc, Guard: guard, ProxyAddr: *addr, Version: Version, Connector: a.conn,
	}

	p := proxy.New(proxy.Options{
		UpstreamBase:  upstreamBase(),
		UpstreamProxy: a.svc.UpstreamProxyFor,
		Selector:      a.svc,
		Clock:         a.svc.Clock,
		Logger:        a.svc.Log,
	})

	mux := http.NewServeMux()
	// Codex traffic. No dashboard session is required here: this is the local client, and
	// it authenticates to us by being on loopback, exactly as codex-lb's provider does.
	mux.Handle("/backend-api/", p)
	mux.Handle("/api/", guard.Wrap(api.Routes()))
	mux.Handle("/", httpapi.Dashboard(guard.Token(), Version))

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 20 * time.Second,
		// No write timeout: a streamed turn legitimately runs long.
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("could not bind %s: %w", *addr, err)
	}

	// The dashboard session token is handed to the page inside the HTML, not through the
	// URL: a URL is logged, kept in browser history and shared by accident.
	url := fmt.Sprintf("http://%s/", *addr)
	fmt.Printf("codexrelay %s\n", Version)
	fmt.Printf("dashboard: %s\n", url)
	fmt.Printf("point Codex at: http://%s/backend-api/codex  (run `codexrelay setup` to do this for you)\n", *addr)
	if *openBrowser {
		_ = open(url)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	model := fs.String("model", "", "model to evaluate against")
	_ = fs.Parse(args)

	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()

	st := a.svc.Registry.Current()
	d := policy.Evaluate(st, policy.Request{Model: *model}, a.svc.Clock.Now())
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(d)
	}

	fmt.Println(d.Summary)
	if d.UnknownEvidence {
		fmt.Println("Some quota evidence is missing, so protection is being held rather than assumed clear.")
	}
	fmt.Println()
	fmt.Printf("%-28s %-10s %s\n", "WORKSPACE", "STATUS", "QUOTA")
	for _, id := range st.Order {
		ws := st.Workspaces[id]
		if ws == nil {
			continue
		}
		status := "ready"
		for _, c := range d.Candidates {
			if c.WorkspaceID == id && !c.Eligible {
				status = string(c.Reason)
			}
		}
		var parts []string
		for m, w := range ws.Windows {
			label := policy.HumanWindow(m)
			part := fmt.Sprintf("%s %.0f%% left", label, w.RemainingPercent())
			if w.ResetsAt != nil {
				part += fmt.Sprintf(" (resets %s)", humanUntil(w.ResetsAt.Sub(a.svc.Clock.Now())))
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			parts = append(parts, "no reading yet")
		}
		fmt.Printf("%-28s %-10s %s\n", truncate(ws.Name, 28), status, strings.Join(parts, "; "))
	}
	for _, n := range d.Notes {
		if n.Comparison != "" {
			fmt.Printf("\n%s\n  %s\n", n.Message, n.Comparison)
		}
	}
	return nil
}

func cmdRules(args []string) error {
	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()
	st := a.svc.Registry.Current()
	if len(st.Rules) == 0 {
		fmt.Println("No rules yet. Create one in the dashboard under Rules.")
		return nil
	}
	name := func(id string) string {
		if ws := st.Workspaces[id]; ws != nil {
			return ws.Name
		}
		return id
	}
	for _, r := range st.Rules {
		state := "on"
		if !r.Enabled {
			state = "off"
		}
		fmt.Printf("[%s] %s\n", state, r.Sentence(name))
	}
	return nil
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7788", "address the service listens on")
	yes := fs.Bool("yes", false, "apply without asking")
	_ = fs.Parse(args)

	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()

	d := integration.Detect()
	fmt.Printf("Codex home:    %s\n", d.CodexHome)
	fmt.Printf("Config file:   %s (exists: %v)\n", d.ConfigPath, d.ConfigExists)
	if d.CodexOnPath {
		fmt.Printf("Codex version: %s\n", d.CodexVersion)
	}
	for _, n := range d.Notes {
		fmt.Println("Note:", n)
	}

	plan, err := integration.BuildPlan(d.ConfigPath, *addr, true)
	if err != nil {
		return err
	}
	if plan.NoChange {
		fmt.Println("\nYour Codex configuration is already set up for codexrelay. Nothing to change.")
		return nil
	}
	fmt.Println("\nProposed change:")
	fmt.Println(plan.Diff)

	if !*yes {
		fmt.Print("Apply this change? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Nothing was changed.")
			return nil
		}
	}
	c, err := integration.Apply(plan, service.ChangeRecorder{Svc: a.svc}, a.svc.Clock.Now())
	if err != nil {
		return err
	}
	fmt.Printf("Applied. A backup of your previous config is at %s\n", c.BackupPath)
	fmt.Println("Run `codexrelay rollback` to undo this.")
	fmt.Println("\nNext: run `codexrelay serve`, open the dashboard, and connect a workspace.")
	return nil
}

func cmdRollback(args []string) error {
	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()
	d := integration.Detect()
	res, err := integration.Rollback(service.ChangeRecorder{Svc: a.svc}, d.ConfigPath, a.svc.Clock.Now())
	if err != nil {
		return err
	}
	fmt.Println(res.Message)
	return nil
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7788", "address the service listens on")
	yes := fs.Bool("yes", false, "apply without asking")
	_ = fs.Parse(args)

	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()

	d := integration.Detect()
	plan, err := integration.PlanMigration(d.ConfigPath, *addr, true)
	if err != nil {
		return err
	}
	if !plan.Detected {
		fmt.Println("No other Codex router was found in your configuration.")
		fmt.Println("If you just want to start using codex-relay, run `codexrelay setup`.")
		return nil
	}

	fmt.Printf("Found %s in %s\n\n", plan.From, plan.ConfigPath)
	fmt.Println("codex-relay will:")
	for _, s := range plan.Steps {
		fmt.Println("  -", s)
	}
	fmt.Println("\nYou will need to do yourself:")
	for _, s := range plan.Manual {
		fmt.Println("  -", s)
	}
	if len(plan.Incompatible) > 0 {
		fmt.Println("\nNot carried over:")
		for _, s := range plan.Incompatible {
			fmt.Println("  -", s)
		}
	}
	if plan.Diff != "" {
		fmt.Println("\nProposed change:")
		fmt.Println(plan.Diff)
	}

	if !*yes {
		fmt.Print("Apply this change? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Nothing was changed.")
			return nil
		}
	}
	build, err := integration.BuildPlan(d.ConfigPath, *addr, true)
	if err != nil {
		return err
	}
	c, err := integration.Apply(build, service.ChangeRecorder{Svc: a.svc}, a.svc.Clock.Now())
	if err != nil {
		return err
	}
	fmt.Printf("\nApplied. Backup: %s\n", c.BackupPath)
	fmt.Println("Undo at any time with `codexrelay rollback`.")
	fmt.Println("Next: run `codexrelay serve` and connect each workspace. Sign-ins are not copied.")
	return nil
}

func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	yes := fs.Bool("yes", false, "remove without asking")
	keepData := fs.Bool("keep-data", false, "undo the Codex config change but keep codex-relay's own data and sign-ins")
	_ = fs.Parse(args)

	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()

	ctx := context.Background()
	d := integration.Detect()
	st := a.svc.Registry.Current()

	bound := 0
	for id := range st.Workspaces {
		threads, terr := a.svc.DB.ThreadsForWorkspace(ctx, id)
		if terr == nil {
			bound += len(threads)
		}
	}
	dataDir, _ := paths.DataDir()
	plan := integration.DescribeUninstall(d.ConfigPath, dataDir, len(st.Workspaces), bound, d.ManagedByUs)

	fmt.Println("Removing codex-relay will:")
	for _, e := range plan.Effects {
		fmt.Println("  -", e)
	}
	fmt.Println("\nIt will NOT:")
	for _, k := range plan.Keeps {
		fmt.Println("  -", k)
	}
	if *keepData {
		fmt.Println("\n--keep-data was given: sign-ins and local data are kept; only the Codex config change is undone.")
	}

	if !*yes {
		fmt.Print("\nProceed? [y/N] ")
		var answer string
		_, _ = fmt.Scanln(&answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			fmt.Println("Nothing was removed.")
			return nil
		}
	}

	res, rerr := integration.Rollback(service.ChangeRecorder{Svc: a.svc}, d.ConfigPath, a.svc.Clock.Now())
	if rerr != nil {
		return fmt.Errorf("could not restore your Codex configuration, so nothing else was removed: %w", rerr)
	}
	fmt.Println("\n" + res.Message)

	if *keepData {
		fmt.Println("Local data and sign-ins were kept, as requested.")
		return nil
	}

	removed, failed := 0, 0
	for id := range st.Workspaces {
		var ref string
		if qerr := a.svc.DB.SQL().QueryRowContext(ctx,
			`SELECT credential_ref FROM workspaces WHERE id = ?`, id).Scan(&ref); qerr != nil || ref == "" {
			continue
		}
		a.svc.Creds.Forget(ref)
		if derr := a.svc.Secrets.Delete(ref); derr != nil {
			failed++
			fmt.Printf("  could not delete the stored sign-in for %s: %v\n", id, derr)
			continue
		}
		removed++
	}
	fmt.Printf("Deleted %d stored sign-in(s) from %s.\n", removed, a.svc.Secrets.Kind())
	if failed > 0 {
		fmt.Printf("%d could not be deleted; remove them from your OS credential store manually.\n", failed)
	}

	a.close()
	if dataDir != "" {
		if rerr := os.RemoveAll(dataDir); rerr != nil {
			fmt.Printf("Could not remove %s: %v\n", dataDir, rerr)
		} else {
			fmt.Printf("Removed %s.\n", dataDir)
		}
	}
	fmt.Println("\ncodex-relay is uninstalled. The executable itself is still on disk; delete it when you like.")
	return nil
}

func cmdDoctor(args []string) error {
	a, err := boot(slog.LevelWarn)
	if err != nil {
		return err
	}
	defer a.close()

	d := integration.Detect()
	h := a.svc.Secrets.Probe()
	schema, _ := a.svc.DB.SchemaVersion(context.Background())
	report := map[string]any{
		"app_version":    Version,
		"os":             runtime.GOOS,
		"arch":           runtime.GOARCH,
		"schema_version": schema,
		"credential_storage": map[string]any{
			"kind": h.Kind, "ok": h.OK, "detail": h.Detail, "remedy": h.Remedy,
		},
		"codex": map[string]any{
			"home": d.CodexHome, "config_exists": d.ConfigExists,
			"on_path": d.CodexOnPath, "version": d.CodexVersion,
			"managed_by_codexrelay": d.ManagedByUs, "existing_provider": d.ExistingProvider,
		},
		"note": "Redacted by design: no account names, emails, tokens or conversation ids.",
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func humanUntil(d time.Duration) string {
	if d <= 0 {
		return "now"
	}
	if h := d.Hours(); h >= 24 {
		return fmt.Sprintf("in %.0f days", h/24)
	}
	return fmt.Sprintf("in %.0f hours", d.Hours())
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func open(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}
