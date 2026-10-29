package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/bb"
	"openfactory/internal/commandapi"
	"openfactory/internal/config"
	"openfactory/internal/dashboard"
	"openfactory/internal/engine"
	"openfactory/internal/executor"
	"openfactory/internal/frontdoor"
	"openfactory/internal/gitrepo"
	"openfactory/internal/intake"
	"openfactory/internal/model"
	"openfactory/internal/notify"
	"openfactory/internal/research"
	"openfactory/internal/store"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var rendered *programCLIError
		if !errors.As(err, &rendered) {
			fmt.Fprintln(os.Stderr, "openfactory:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, out, errout io.Writer) (result error) {
	fs := flag.NewFlagSet("openfactory", flag.ContinueOnError)
	fs.SetOutput(errout)
	path := fs.String("config", "config/openfactory.yaml", "operator configuration file")
	fs.Usage = func() {
		fmt.Fprintln(errout, "Usage: openfactory [--config FILE] COMMAND [ARGS]\nCommands: run, doctor, enqueue FILE..., approve ID --revision HASH, status [ID],\n  questions, retry ID, cancel ID, pause, resume, events [ID], evidence ID,\n  backup FILE, plan PROJECT GOAL.md, program COMMAND [ARGS],\n  operator COMMAND [ARGS], intake COMMAND [ARGS], config intake\nCommands return JSON except run logs and intake export without --output (raw source).\nNo command implicitly approves deployment.")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return errors.New("a command is required")
	}
	if args[0] == "program" {
		defer func() {
			if result != nil {
				result = renderProgramError(errout, result)
			}
		}()
	}
	if args[0] == "executor" {
		ef := flag.NewFlagSet("executor", flag.ContinueOnError)
		ef.SetOutput(errout)
		socket := ef.String("socket", "/run/openfactory-checks/check.sock", "private check socket")
		root := ef.String("root", "/workspaces", "allowed workspace root")
		journalRoot := ef.String("journal-root", "", "private durable experiment journal (required for experiments)")
		artifactsRoot := ef.String("artifacts-root", "", "controller CAS directory shared only with executor")
		declaredImage := ef.String("declared-image-identity", "", "operator-declared image pin; not observed OCI identity")
		var prerequisites stringFlags
		ef.Var(&prerequisites, "prerequisite", "operator-attested experiment prerequisite (repeatable)")
		if e := ef.Parse(args[1:]); e != nil {
			return e
		}
		if ef.NArg() != 0 {
			return errors.New("unexpected executor arguments")
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if *journalRoot == "" {
			if *artifactsRoot != "" || *declaredImage != "" || len(prerequisites) != 0 {
				return errors.New("experiment options require --journal-root")
			}
			return executor.Serve(ctx, *socket, *root)
		}
		policy, e := config.Load(*path)
		if e != nil {
			return e
		}
		if *artifactsRoot == "" {
			return errors.New("--journal-root requires explicit --artifacts-root")
		}
		return serveExperimentExecutor(ctx, *path, policy, *socket, *root, executor.Options{
			JournalDir: *journalRoot, ArtifactRoot: *artifactsRoot,
			Profiles:              policy.Discovery.ExperimentProfiles,
			Limits:                policy.Discovery.ExperimentLimits,
			DeclaredImageIdentity: *declaredImage, Prerequisites: prerequisites,
		})
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	if args[0] == "config" {
		if len(args) != 2 || args[1] != "intake" {
			return errors.New("usage: config intake")
		}
		return json.NewEncoder(out).Encode(map[string]any{"intake": c.Intake, "policy_hash": c.PolicyHash})
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(c.StateDir, "openfactory.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	emit := func(v any) error { enc := json.NewEncoder(out); enc.SetIndent("", "  "); return enc.Encode(v) }
	command, rest := args[0], args[1:]
	exact := func(n int) error {
		if len(rest) != n {
			return fmt.Errorf("%s requires %d argument(s)", command, n)
		}
		return nil
	}
	switch command {
	case "program":
		return runProgram(ctx, c, db, rest, out)
	case "operator":
		return runOperator(ctx, c, db, rest, out, errout)
	case "intake":
		return runIntake(ctx, c, db, rest, out)
	case "run":
		rf := flag.NewFlagSet("run", flag.ContinueOnError)
		rf.SetOutput(errout)
		address := rf.String("dashboard-listen", "127.0.0.1:38887", "read-only dashboard IP:port (empty disables)")
		allowNonLoopback := rf.Bool("dashboard-allow-non-loopback", false, "explicitly allow a non-loopback dashboard bind")
		researchAddress := rf.String("research-listen", c.Discovery.BrokerBind, "private authenticated research broker IP:port")
		allowResearchNonLoopback := rf.Bool("research-allow-non-loopback", c.Discovery.AllowNonLoopback, "explicitly allow a non-loopback private research broker bind")
		commandAddress := rf.String("command-listen", c.Intake.Bind, "private BB + authenticated intake frontdoor IP:port (intake.enabled required)")
		allowCommandNonLoopback := rf.Bool("command-allow-non-loopback", c.Intake.AllowNonLoopback, "explicitly allow a non-loopback private frontdoor bind")
		if err := rf.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if rf.NArg() != 0 {
			return errors.New("unexpected run arguments")
		}
		return serve(ctx, *path, c, db, errout, *address, *allowNonLoopback, *researchAddress, *allowResearchNonLoopback, *commandAddress, *allowCommandNonLoopback)
	case "doctor":
		if err := exact(0); err != nil {
			return err
		}
		if err = gitrepo.CheckCapabilities(ctx); err != nil {
			return err
		}
		b := bb.New(c)
		if err = b.Doctor(ctx); err != nil {
			return fmt.Errorf("BB readiness: %w; when BB_APP_URL uses a separate public origin, review explicit bb_host_authority in examples/intake-private-https.txt (HTTP Host only; never a DNS/TLS override)", err)
		}
		for name, p := range c.Projects {
			for _, file := range []string{p.Auth.TokenFile, p.Auth.SSHKeyFile, p.Auth.KnownHostsFile} {
				if file != "" {
					info, e := os.Stat(file)
					if e != nil || !info.Mode().IsRegular() {
						return fmt.Errorf("project %s has an unavailable credential/trust file", name)
					}
				}
			}
		}
		return emit(map[string]any{
			"status": "ready", "projects": len(c.Projects), "policy_hash": c.PolicyHash,
			"model_authentication": "not_exercised", "billed_token_accounting": "unavailable",
			"bb_transport": map[string]any{
				"url":                               c.BBURL,
				"host_authority_override":           c.BBHostAuthority,
				"empty_override_uses_url_authority": true,
				"tls_identity_uses_url":             true,
			},
			"discovery": map[string]any{
				"broker_url":                       c.Discovery.BrokerURL,
				"broker_capability_authentication": "required",
				"broker_connectivity":              "not_exercised",
				"public_search_provider":           "keyless_parallel_mcp_only",
				"implicit_paid_search_fallback":    false,
				"configured_experiment_profiles":   len(c.Discovery.ExperimentProfiles),
				"executor_journal_capability":      "not_exercised",
				"runtime_image_identity":           "not_observed",
				"experiment_binary_identity":       "recorded_by_runner_per_command",
				"resource_limits_scope":            "executor_container_not_per_run",
			},
		})
	case "enqueue":
		if len(rest) == 0 {
			return errors.New("enqueue requires at least one feature file")
		}
		features := make([]model.Feature, 0, len(rest))
		for _, file := range rest {
			data, e := readBounded(file, 1<<20)
			if e != nil {
				return e
			}
			f, e := intake.Parse(data)
			if e != nil {
				return fmt.Errorf("%s: %w", file, e)
			}
			if e = validateFeatureProject(c, f); e != nil {
				return e
			}
			f.PolicyHash = c.PolicyHash
			features = append(features, f)
		}
		// Import prerequisites first; cycles/missing prerequisites remain explicit errors.
		ordered, err := orderFeatures(features)
		if err != nil {
			return err
		}
		results := make([]model.Feature, 0, len(ordered))
		for _, f := range ordered {
			v, e := db.Import(ctx, f)
			if e != nil {
				return e
			}
			results = append(results, v)
		}
		return emit(results)
	case "approve":
		if len(rest) != 3 || rest[1] != "--revision" {
			return errors.New("usage: approve ID --revision EXACT_HASH")
		}
		f, err := db.Get(ctx, rest[0])
		if err != nil {
			return err
		}
		if err = validateFeatureProject(c, f); err != nil {
			return err
		}
		if err = db.Approve(ctx, rest[0], rest[2], c.PolicyHash, model.TrustedLocalAudit()); err != nil {
			return err
		}
		f, err = db.Get(ctx, rest[0])
		if err != nil {
			return err
		}
		return emit(f)
	case "status":
		if len(rest) > 1 {
			return errors.New("usage: status [ID]")
		}
		if len(rest) == 1 {
			f, e := db.Get(ctx, rest[0])
			if e != nil {
				return e
			}
			return emit(f)
		}
		list, e := db.List(ctx)
		if e != nil {
			return e
		}
		paused, e := db.Paused(ctx)
		if e != nil {
			return e
		}
		return emit(map[string]any{"paused": paused, "features": list})
	case "questions":
		if err := exact(0); err != nil {
			return err
		}
		list, e := db.List(ctx)
		if e != nil {
			return e
		}
		questions := []model.Feature{}
		for _, f := range list {
			if f.State == "blocked" || f.State == "awaiting_approval" || f.State == "uncertain" {
				questions = append(questions, f)
			}
		}
		return emit(questions)
	case "retry", "cancel":
		if err := exact(1); err != nil {
			return err
		}
		if command == "retry" {
			err = db.Retry(ctx, rest[0], model.TrustedLocalAudit())
		} else {
			err = db.Cancel(ctx, rest[0], model.TrustedLocalAudit())
		}
		if err != nil {
			return err
		}
		f, e := db.Get(ctx, rest[0])
		if e != nil {
			return e
		}
		return emit(f)
	case "resolve-failed":
		if len(rest) != 5 || rest[1] != "--attempt" || rest[3] != "--receipt" {
			return errors.New("usage: resolve-failed ID --attempt EXACT_ATTEMPT_ID --receipt 'observed external execution stopped/settled; evidence reference'")
		}
		if err = db.ResolveFailed(ctx, rest[0], rest[2], rest[4]); err != nil {
			return err
		}
		f, e := db.Get(ctx, rest[0])
		if e != nil {
			return e
		}
		return emit(f)
	case "pause", "resume":
		if err := exact(0); err != nil {
			return err
		}
		if err = db.SetPaused(ctx, command == "pause"); err != nil {
			return err
		}
		return emit(map[string]bool{"paused": command == "pause"})
	case "events":
		if len(rest) > 1 {
			return errors.New("usage: events [ID]")
		}
		id := ""
		if len(rest) == 1 {
			id = rest[0]
		}
		events, e := db.Events(ctx, id)
		if e != nil {
			return e
		}
		return emit(events)
	case "evidence":
		if err := exact(1); err != nil {
			return err
		}
		evidence, e := db.Evidence(ctx, rest[0])
		if e != nil {
			return e
		}
		return emit(evidence)
	case "backup":
		if err := exact(1); err != nil {
			return err
		}
		if err = db.Backup(ctx, rest[0]); err != nil {
			return err
		}
		return emit(map[string]string{"backup": rest[0]})
	case "plan":
		if err := exact(2); err != nil {
			return err
		}
		if _, ok := c.Projects[rest[0]]; !ok {
			return errors.New("unknown project")
		}
		body, e := readBounded(rest[1], c.MaxPromptBytes/2)
		if e != nil {
			return e
		}
		if len(strings.TrimSpace(string(body))) == 0 {
			return errors.New("goal is empty")
		}
		nonce := make([]byte, 6)
		if _, e = rand.Read(nonce); e != nil {
			return e
		}
		f, e := intake.NewPlan("plan-"+hex.EncodeToString(nonce), rest[0], "Plan "+filepath.Base(rest[1]), string(body))
		if e != nil {
			return e
		}
		f.PolicyHash = c.PolicyHash
		saved, e := db.Import(ctx, f)
		if e != nil {
			return e
		}
		if e = db.Approve(ctx, saved.ID, saved.Revision, c.PolicyHash, model.TrustedLocalAudit()); e != nil {
			return e
		}
		saved, e = db.Get(ctx, saved.ID)
		if e != nil {
			return e
		}
		return emit(saved)
	default:
		fs.Usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func serve(parent context.Context, path string, c model.Config, db *store.Store, log io.Writer, dashboardAddr string, allowNonLoopback bool, researchAddr string, allowResearchNonLoopback bool, commandAddr string, allowCommandNonLoopback bool) error {
	if os.Getenv("OPENFACTORY_REQUIRE_CHECK_SOCKET") == "1" && c.CheckSocket == "" {
		return errors.New("container execution requires an isolated check_socket")
	}
	if dashboardAddr != "" {
		if err := dashboard.ValidateListenAddress(dashboardAddr, allowNonLoopback); err != nil {
			return err
		}
	}
	if researchAddr != "" {
		if err := dashboard.ValidateListenAddress(researchAddr, allowResearchNonLoopback); err != nil {
			return fmt.Errorf("private research broker: %w", err)
		}
	}
	if !c.Intake.Enabled {
		commandAddr = ""
	} else {
		if err := config.ValidateIntakePolicy(c.Intake); err != nil {
			return err
		}
		if commandAddr == "" {
			return errors.New("enabled intake requires a dedicated command listener")
		}
		if err := dashboard.ValidateListenAddress(commandAddr, allowCommandNonLoopback); err != nil {
			return fmt.Errorf("private intake command API: %w", err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(c.StateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another openfactory daemon owns this state directory")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	var listener net.Listener
	components := 3
	if dashboardAddr != "" {
		listener, err = net.Listen("tcp", dashboardAddr)
		if err != nil {
			return fmt.Errorf("listen for openfactory dashboard: %w", err)
		}
		defer listener.Close()
		components++
	}
	var researchListener net.Listener
	if researchAddr != "" {
		researchListener, err = net.Listen("tcp", researchAddr)
		if err != nil {
			return fmt.Errorf("listen for private research broker: %w", err)
		}
		defer researchListener.Close()
		components++
	}
	var commandListener net.Listener
	var commandHandler http.Handler
	if commandAddr != "" {
		commandListener, err = net.Listen("tcp", commandAddr)
		if err != nil {
			return fmt.Errorf("listen for private intake command API: %w", err)
		}
		defer commandListener.Close()
		commandHandler, err = commandapi.New(c, db)
		if err != nil {
			return fmt.Errorf("initialize private intake command API: %w", err)
		}
		commandHandler, err = frontdoor.New(c, commandHandler)
		if err != nil {
			return fmt.Errorf("initialize private BB/intake frontdoor: %w", err)
		}
		components++
	}
	if err = gitrepo.CheckCapabilities(parent); err != nil {
		return err
	}
	if err = db.PrepareIntegrationRecovery(parent, c); err != nil {
		return fmt.Errorf("recover delivery ownership: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	errorsCh := make(chan error, components)
	go func() { errorsCh <- engine.New(c, db, bb.New(c), gitrepo.New(c)).Run(ctx) }()
	go func() { errorsCh <- notify.New(c, db).Run(ctx) }()
	go func() {
		ticker := time.NewTicker(time.Duration(c.PollSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				errorsCh <- nil
				return
			case <-ticker.C:
				next, e := config.Load(path)
				if e != nil {
					errorsCh <- errors.New("operator configuration became invalid; admission stopped")
					return
				}
				if next.PolicyHash != c.PolicyHash {
					errorsCh <- errors.New("operator policy changed; restart required to reconcile under new policy")
					return
				}
			}
		}
	}()
	if listener != nil {
		go func() {
			errorsCh <- serveDashboard(ctx, listener, dashboard.NewForAddress(c, db, listener.Addr().String()))
		}()
		fmt.Fprintf(log, "openfactory dashboard listening on %s\n", listener.Addr())
	}
	if researchListener != nil {
		broker := research.New(c, db, artifacts.New(filepath.Join(c.StateDir, "artifacts")))
		go func() {
			errorsCh <- serveHTTPComponent(ctx, researchListener, broker.Handler(), "private research broker", 35*time.Second, 30*time.Second)
		}()
		fmt.Fprintf(log, "openfactory private research broker listening on %s\n", researchListener.Addr())
	}
	if commandListener != nil {
		go func() {
			errorsCh <- serveHTTPComponent(ctx, commandListener, commandHandler, "private BB/intake frontdoor", 35*time.Second, 60*time.Second)
		}()
		fmt.Fprintf(log, "openfactory private BB/intake frontdoor listening on %s\n", commandListener.Addr())
	}
	fmt.Fprintln(log, "openfactory running; idle polling does not call a model")
	var first error
	// Cancel on the first exit, then drain every supervised component before
	// releasing the process lock/closing SQLite.
	for remaining := components; remaining > 0; remaining-- {
		e := <-errorsCh
		cancel()
		if first == nil && e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, parent.Err()) {
			first = e
		}
	}
	return first
}

func serveDashboard(ctx context.Context, listener net.Listener, handler http.Handler) error {
	return serveHTTPComponent(ctx, listener, handler, "dashboard", 30*time.Second, 60*time.Second)
}

func serveHTTPComponent(ctx context.Context, listener net.Listener, handler http.Handler, name string, ioTimeout, idleTimeout time.Duration) error {
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	var requests sync.RWMutex
	trackedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.RLock()
		defer requests.RUnlock()
		if requestCtx.Err() != nil {
			http.Error(w, "openfactory "+name+" is shutting down", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	})
	server := &http.Server{
		Handler:           trackedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       ioTimeout,
		WriteTimeout:      ioTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-served:
	case <-ctx.Done():
	}
	cancelRequests()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		// Slow clients must not keep the process lock or database open forever.
		server.Close()
	}
	if serveErr == nil {
		serveErr = <-served
	}
	// Close interrupts client I/O but does not itself wait for handlers. Keep
	// SQLite and the process lock alive until all admitted reads have returned.
	requests.Lock()
	requests.Unlock()
	normalClose := errors.Is(serveErr, http.ErrServerClosed) || ctx.Err() != nil && errors.Is(serveErr, net.ErrClosed)
	if !normalClose {
		return fmt.Errorf("serve openfactory %s: %w", name, serveErr)
	}
	if shutdownErr != nil && !(ctx.Err() != nil && errors.Is(shutdownErr, net.ErrClosed)) {
		return fmt.Errorf("shut down openfactory %s: %w", name, shutdownErr)
	}
	return nil
}

func readBounded(path string, max int) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if e != nil {
		return nil, e
	}
	if len(data) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, max)
	}
	return data, nil
}

func validateFeatureProject(c model.Config, f model.Feature) error {
	p, ok := c.Projects[f.Project]
	if !ok || !p.Trusted {
		return fmt.Errorf("feature %s references an unenrolled project", f.ID)
	}
	for _, name := range f.Checks {
		if _, ok := p.Checks[name]; !ok {
			return fmt.Errorf("feature %s names unknown check %q", f.ID, name)
		}
	}
	if f.CompletionTarget == "staging" || f.CompletionTarget == "production" {
		r, ok := p.Releases[f.CompletionTarget]
		if !ok || !r.Allowed {
			return fmt.Errorf("feature %s has no allowlisted %s release profile", f.ID, f.CompletionTarget)
		}
	}
	if f.CompletionTarget == "pull_request" && (p.GitHubRepository == "" || p.Auth.TokenFile == "") {
		return errors.New("pull_request target requires github_repository and an API token_file (also for SSH push)")
	}
	return nil
}

func orderFeatures(features []model.Feature) ([]model.Feature, error) {
	byID := map[string]model.Feature{}
	for _, f := range features {
		if _, ok := byID[f.ID]; ok {
			return nil, fmt.Errorf("duplicate feature %s", f.ID)
		}
		byID[f.ID] = f
	}
	state := map[string]int{}
	result := make([]model.Feature, 0, len(features))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("dependency cycle at %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		f := byID[id]
		for _, dep := range f.DependsOn {
			if _, ok := byID[dep]; ok {
				if e := visit(dep); e != nil {
					return e
				}
			}
		}
		state[id] = 2
		result = append(result, f)
		return nil
	}
	for _, f := range features {
		if e := visit(f.ID); e != nil {
			return nil, e
		}
	}
	return result, nil
}
