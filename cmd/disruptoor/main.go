// Command disruptoor is a privileged sidecar that applies network
// disruptions (partitions, shaping) to other Docker containers in an
// enclave, controllable via HTTP. See disruptoor.md for the full design.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"net/http"

	"github.com/ethpandaops/disruptoor/internal/api"
	"github.com/ethpandaops/disruptoor/internal/backend/conntrack"
	"github.com/ethpandaops/disruptoor/internal/backend/iptables"
	"github.com/ethpandaops/disruptoor/internal/backend/tc"
	"github.com/ethpandaops/disruptoor/internal/config"
	"github.com/ethpandaops/disruptoor/internal/discovery"
	"github.com/ethpandaops/disruptoor/internal/netns"
	"github.com/ethpandaops/disruptoor/internal/runner"
	"github.com/ethpandaops/disruptoor/internal/state"
	"github.com/ethpandaops/disruptoor/internal/webui"
	"github.com/ethpandaops/disruptoor/internal/webui/eventlog"
)

var version = "dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type runFlags struct {
	addr            string
	enclaveID       string
	enclaveLabelKey string
	labelPrefix     string
	logLevel        string
	logFormat       string
	configPath      string
	disableWebUI    bool
	webUIEventSize  int
	webUIDebug      bool
}

func newRoot() *cobra.Command {
	var f runFlags
	root := &cobra.Command{
		Use:     "disruptoor",
		Short:   "Privileged Docker sidecar for network disruptions",
		Version: version,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), f)
		},
	}
	root.PersistentFlags().StringVar(&f.addr, "addr", ":7700",
		"HTTP listen address")
	root.PersistentFlags().StringVar(&f.enclaveID, "enclave-id", "",
		"Override enclave UUID; if empty, learnt from self-container labels")
	root.PersistentFlags().StringVar(&f.enclaveLabelKey, "enclave-label-key",
		discovery.DefaultEnclaveLabelKey,
		"Docker label key whose value identifies the enclave")
	root.PersistentFlags().StringVar(&f.labelPrefix, "label-prefix",
		discovery.DefaultLabelPrefix,
		"Prefix prepended to selector keys without a dot")
	root.PersistentFlags().StringVar(&f.logLevel, "log-level", "info",
		"Log level: debug, info, warn, error")
	root.PersistentFlags().StringVar(&f.logFormat, "log-format", "json",
		"Log format: json or text")
	root.PersistentFlags().StringVar(&f.configPath, "config", "",
		"Path to initial state file (.yaml/.yml/.json); applied before HTTP serving begins")
	root.PersistentFlags().BoolVar(&f.disableWebUI, "disable-webui", false,
		"Disable the embedded web UI (it shares the API listener when enabled)")
	root.PersistentFlags().IntVar(&f.webUIEventSize, "webui-event-buffer", 200,
		"Size of the in-memory event ring shown by the web UI")
	root.PersistentFlags().BoolVar(&f.webUIDebug, "webui-debug", false,
		"Disable template caching in the web UI (reload from embed.FS each request)")

	root.SetContext(rootContext())
	return root
}

func rootContext() context.Context {
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		// the second signal hard-kills the process. NotifyContext already
		// stops listening after the first; we install a fresh handler.
		hard := make(chan os.Signal, 1)
		signal.Notify(hard, syscall.SIGINT, syscall.SIGTERM)
		<-hard
		os.Exit(130)
	}()
	_ = cancel // released when the process exits
	return ctx
}

func run(ctx context.Context, f runFlags) error {
	logger, err := newLogger(f.logLevel, f.logFormat)
	if err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	logger.InfoContext(ctx, "disruptoor starting",
		slog.String("addr", f.addr),
		slog.String("enclave_label_key", f.enclaveLabelKey),
		slog.String("label_prefix", f.labelPrefix))

	disc, err := discovery.NewService(logger, discovery.Config{
		EnclaveLabelKey:   f.enclaveLabelKey,
		EnclaveLabelValue: f.enclaveID,
		LabelPrefix:       f.labelPrefix,
	})
	if err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	if err := disc.Start(ctx); err != nil {
		return fmt.Errorf("discovery start: %w", err)
	}
	defer disc.Stop()

	enterer := netns.New(runner.New())
	listAllUserServices := func(ctx context.Context) ([]discovery.Container, error) {
		return disc.Resolve(ctx, state.Selector{All: true})
	}

	iptSvc := iptables.NewService(logger, enterer, listAllUserServices)
	if err := iptSvc.Start(ctx); err != nil {
		return fmt.Errorf("iptables start: %w", err)
	}
	defer iptSvc.Stop()

	tcSvc := tc.NewService(logger, enterer, listAllUserServices)
	if err := tcSvc.Start(ctx); err != nil {
		return fmt.Errorf("tc start: %w", err)
	}
	defer tcSvc.Stop()

	ctSvc := conntrack.NewService(logger, enterer)

	// Break the cycle between api and webui (each wants a reference to the
	// other) with a forward-declared variable: the api gets a closure that
	// reads webUISvc, which is assigned after api.NewService returns. The
	// closure is only invoked from Start, well after both are wired.
	var (
		webUISvc     webui.Service
		events       *eventlog.Ring
		onEventFn    func(api.Event)
		extraRoutes  func(*http.ServeMux)
		webUIEnabled = !f.disableWebUI
	)
	if webUIEnabled {
		events = eventlog.New(eventlog.Config{Size: f.webUIEventSize})
		onEventFn = events.Append
		extraRoutes = func(mux *http.ServeMux) {
			if webUISvc != nil {
				webUISvc.RegisterRoutes(mux)
			}
		}
	}

	apiSvc, err := api.NewService(logger, api.Config{
		Addr:        f.addr,
		Discovery:   disc,
		Iptables:    iptSvc,
		TC:          tcSvc,
		Conntrack:   ctSvc,
		OnEvent:     onEventFn,
		ExtraRoutes: extraRoutes,
	})
	if err != nil {
		return fmt.Errorf("api: %w", err)
	}

	if webUIEnabled {
		webUISvc, err = webui.NewService(logger, webui.Config{
			SiteName:  "disruptoor",
			Version:   version,
			BuildTime: time.Now().UTC().Format("2006-01-02T15:04:05Z"),
			Debug:     f.webUIDebug,
			State:     apiSvc,
			Discovery: disc,
			Events:    events,
		})
		if err != nil {
			return fmt.Errorf("webui: %w", err)
		}
		logger.InfoContext(ctx, "web UI enabled",
			slog.String("addr", f.addr),
			slog.Bool("debug", f.webUIDebug),
			slog.Int("event_buffer", f.webUIEventSize))
	}

	if f.configPath != "" {
		initial, err := config.Load(f.configPath)
		if err != nil {
			return fmt.Errorf("load --config %s: %w", f.configPath, err)
		}
		if err := apiSvc.Apply(ctx, initial); err != nil {
			return fmt.Errorf("apply initial state from %s: %w", f.configPath, err)
		}
		logger.InfoContext(ctx, "applied initial state from --config",
			slog.String("path", f.configPath),
			slog.Int("partitions", len(initial.Partitions)),
			slog.Int("shaping", len(initial.Shaping)))
	}

	if err := apiSvc.Start(ctx); err != nil {
		return fmt.Errorf("api start: %w", err)
	}

	<-ctx.Done()
	logger.InfoContext(ctx, "shutting down")

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = apiSvc.Stop()
	_ = tcSvc.Clear(stopCtx)
	_ = iptSvc.Clear(stopCtx)
	return nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lvl}
	switch format {
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	case "text":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}
