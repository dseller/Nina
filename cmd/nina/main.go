// Command nina is an OpenAPI-native API gateway.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rivencove/nina/internal/admin"
	"github.com/rivencove/nina/internal/chain"
	"github.com/rivencove/nina/internal/config"
	"github.com/rivencove/nina/internal/mw/apikey"
	"github.com/rivencove/nina/internal/mw/authjwt"
	"github.com/rivencove/nina/internal/mw/cors"
	"github.com/rivencove/nina/internal/mw/headers"
	"github.com/rivencove/nina/internal/mw/ratelimit"
	"github.com/rivencove/nina/internal/mw/validate"
	"github.com/rivencove/nina/internal/observ"
	"github.com/rivencove/nina/internal/runtime"
	"github.com/rivencove/nina/internal/specsrc"
	"github.com/spf13/cobra"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := rootCmd().Execute(); err != nil {
		// Cobra has already printed the error.
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "nina",
		Short:         "An OpenAPI-native API gateway",
		SilenceUsage:  true,
		SilenceErrors: false,
		Version:       version,
	}
	root.AddCommand(runCmd(), checkCmd(), specCmd())
	return root
}

// registry wires every compiled-in middleware. Adding one means adding a line
// here and nothing else.
func registry() *chain.Registry {
	r := chain.NewRegistry()
	r.Register("validate", validate.New)
	r.Register("jwt", authjwt.New)
	r.Register("apikey", apikey.New)
	r.Register("ratelimit", ratelimit.New)
	r.Register("cors", cors.New)
	r.Register("headers", headers.New)
	return r
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("invalid log level %q", level)
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		h = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q, expected json or text", format)
	}
	return slog.New(h), nil
}

func runCmd() *cobra.Command {
	var (
		cfgPath   string
		logLevel  string
		logFormat string
		cacheDir  string
		drain     time.Duration
	)
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the gateway",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := newLogger(logLevel, logFormat)
			if err != nil {
				return err
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cacheDir == "" {
				cacheDir = filepath.Join(os.TempDir(), "nina-spec-cache")
			}

			reg := prometheus.NewRegistry()
			metrics := observ.New(reg, version)
			deps := runtime.Deps{
				Logger:   log,
				Metrics:  metrics,
				Fetcher:  specsrc.New(cacheDir),
				Registry: registry(),
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			abs, _ := filepath.Abs(cfgPath)
			srv, err := runtime.NewServer(ctx, cfg, deps, runtime.ServerOptions{
				ConfigPath: abs,
				DrainGrace: drain,
			})
			if err != nil {
				return err
			}
			srv.Watch(ctx)
			go watchSIGHUP(ctx, srv, log)

			rt := srv.Current()
			log.Info("gateway starting",
				"version", version,
				"listen", cfg.Server.Listen,
				"routes", len(rt.Table.Routes),
				"backends", len(cfg.Backends))
			for _, r := range rt.Table.Routes {
				log.Debug("route", "method", r.Method, "path", r.GatewayPath,
					"backend", r.Backend.Name, "operation", r.OperationID)
			}

			gateway := &http.Server{
				Addr:              cfg.Server.Listen,
				Handler:           srv,
				ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout.Std(),
				WriteTimeout:      cfg.Server.WriteTimeout.Std(),
				IdleTimeout:       cfg.Server.IdleTimeout.Std(),
			}
			servers := []*http.Server{gateway}

			if cfg.Server.Admin.Listen != "" {
				adminSrv := &http.Server{
					Addr: cfg.Server.Admin.Listen,
					Handler: admin.Handler(admin.Options{
						Server: srv, Gatherer: reg, Version: version, Docs: cfg.Server.Admin.Docs,
					}),
					ReadHeaderTimeout: 5 * time.Second,
				}
				servers = append(servers, adminSrv)
				log.Info("admin listener starting", "listen", cfg.Server.Admin.Listen, "docs", cfg.Server.Admin.Docs)
			}

			errCh := make(chan error, len(servers))
			for _, s := range servers {
				go func(s *http.Server) {
					if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
						errCh <- fmt.Errorf("listen on %s: %w", s.Addr, err)
					}
				}(s)
			}

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				log.Info("shutting down")
			}

			// Stop accepting, then let in-flight requests finish.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), drain)
			defer cancel()
			for _, s := range servers {
				_ = s.Shutdown(shutdownCtx)
			}
			if rt := srv.Current(); rt != nil {
				rt.Retire(drain)
			}
			log.Info("stopped")
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "nina.yaml", "path to the configuration file")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "debug, info, warn or error")
	cmd.Flags().StringVar(&logFormat, "log-format", "json", "json or text")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "where to cache fetched specs")
	cmd.Flags().DurationVar(&drain, "drain-grace", 30*time.Second, "how long to let in-flight requests finish")
	return cmd
}

func watchSIGHUP(ctx context.Context, srv *runtime.Server, log *slog.Logger) {
	ch := make(chan os.Signal, 1)
	notifyHUP(ch)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			log.Info("SIGHUP received; reloading")
			_ = srv.Reload(ctx, "SIGHUP")
		}
	}
}

func checkCmd() *cobra.Command {
	var cfgPath string
	var cacheDir string
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate the configuration and every upstream spec, then exit",
		Long: "Loads the configuration, fetches each upstream document, builds the route " +
			"table and reports collisions. Exits non-zero on any problem, which makes it " +
			"usable as a CI gate.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := newLogger("warn", "text")
			if err != nil {
				return err
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cacheDir == "" {
				cacheDir = filepath.Join(os.TempDir(), "nina-spec-cache")
			}
			reg := prometheus.NewRegistry()
			rt, err := runtime.Build(cmd.Context(), cfg, runtime.Deps{
				Logger:   log,
				Metrics:  observ.New(reg, version),
				Fetcher:  specsrc.New(cacheDir),
				Registry: registry(),
			}, 0)
			if err != nil {
				return err
			}
			defer rt.Retire(0)

			fmt.Fprintf(cmd.OutOrStdout(), "ok: %d routes across %d backends\n",
				len(rt.Table.Routes), len(cfg.Backends))
			for _, r := range rt.Table.Routes {
				fmt.Fprintf(cmd.OutOrStdout(), "  %-7s %-40s -> %s %s\n",
					r.Method, r.GatewayPath, r.Backend.Name, r.UpstreamPath)
			}
			if len(rt.Table.Degraded) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: serving cached specs for: %v\n", rt.Table.Degraded)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&cfgPath, "config", "c", "nina.yaml", "path to the configuration file")
	cmd.Flags().StringVar(&cacheDir, "cache-dir", "", "where to cache fetched specs")
	return cmd
}

func specCmd() *cobra.Command {
	spec := &cobra.Command{Use: "spec", Short: "Work with the merged OpenAPI document"}

	var cfgPath, out, format, cacheDir string
	build := &cobra.Command{
		Use:   "build",
		Short: "Render the merged OpenAPI document",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log, err := newLogger("error", "text")
			if err != nil {
				return err
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			if cacheDir == "" {
				cacheDir = filepath.Join(os.TempDir(), "nina-spec-cache")
			}
			reg := prometheus.NewRegistry()
			rt, err := runtime.Build(cmd.Context(), cfg, runtime.Deps{
				Logger:   log,
				Metrics:  observ.New(reg, version),
				Fetcher:  specsrc.New(cacheDir),
				Registry: registry(),
			}, 0)
			if err != nil {
				return err
			}
			defer rt.Retire(0)

			var data []byte
			switch format {
			case "yaml":
				data = rt.Table.SpecYAML
			case "json":
				data = rt.Table.SpecJSON
			default:
				return fmt.Errorf("invalid format %q, expected yaml or json", format)
			}
			if out == "" || out == "-" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			return os.WriteFile(out, data, 0o644)
		},
	}
	build.Flags().StringVarP(&cfgPath, "config", "c", "nina.yaml", "path to the configuration file")
	build.Flags().StringVarP(&out, "out", "o", "-", "output file, or - for stdout")
	build.Flags().StringVarP(&format, "format", "f", "yaml", "yaml or json")
	build.Flags().StringVar(&cacheDir, "cache-dir", "", "where to cache fetched specs")

	spec.AddCommand(build)
	return spec
}
