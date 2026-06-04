package bot

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/joeyciechanowicz/eve-bot/action"
	"github.com/joeyciechanowicz/eve-bot/internal/enrich"
	"github.com/joeyciechanowicz/eve-bot/internal/enrich/names"
	"github.com/joeyciechanowicz/eve-bot/internal/pipeline"
	"github.com/joeyciechanowicz/eve-bot/internal/rules"
	"github.com/joeyciechanowicz/eve-bot/internal/store"
	"github.com/joeyciechanowicz/eve-bot/source"
)

// Run loads cfgPath, builds the pipeline, and blocks until ctx is cancelled.
// Sources and actions must already be registered (via blank imports of their
// packages or of bot/defaults for the built-in set).
func Run(ctx context.Context, cfgPath string) error {
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return RunConfig(ctx, cfg)
}

// RunConfig is like Run but takes an already-parsed Config. Useful for tests
// and for callers that assemble the config programmatically.
func RunConfig(ctx context.Context, cfg *Config) error {
	if cfg.Debug {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	st, err := store.Open(cfg.Store.Path)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer st.Close()
	go st.RunJanitor(ctx, cfg.Store.JanitorInterval, cfg.Store.ActionHistoryTTL)

	httpClient := &http.Client{Timeout: 15 * time.Second}

	p, err := buildPipeline(cfg, st, httpClient)
	if err != nil {
		return fmt.Errorf("build pipeline: %w", err)
	}
	slog.Info("eve-bot starting", "sources", len(p.Sources), "rules", len(cfg.Rules.Rules))

	if err := p.Run(ctx); err != nil {
		return err
	}
	slog.Info("eve-bot stopped")
	return nil
}

func buildPipeline(cfg *Config, st *store.Store, hc *http.Client) (*pipeline.Pipeline, error) {
	handlers := action.BuildHandlers(action.Deps{
		HTTPClient: hc,
		FactWriter: st,
	})

	srcDeps := source.Deps{HTTPClient: hc, Checkpointer: st}
	srcs := make([]source.Source, 0, len(cfg.Sources))
	for _, sc := range cfg.Sources {
		s, err := source.Build(sc.Type, sc.Name, sc.Params, srcDeps)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", sc.Name, err)
		}
		srcs = append(srcs, s)
	}

	ruleSet, err := rules.Compile(cfg.Rules.Mode, cfg.Rules.Rules)
	if err != nil {
		return nil, fmt.Errorf("rules: %w", err)
	}

	disp := action.New(handlers, st, cfg.Retry.MaxRetries, cfg.Retry.BaseBackoff, cfg.Retry.MaxBackoff)

	enrichers := enrich.Chain{
		names.New(hc, st, cfg.Enrich.Names.CacheTTL, cfg.Enrich.Names.BaseURL),
	}

	return &pipeline.Pipeline{
		Sources:    srcs,
		Enrichers:  enrichers,
		Rules:      ruleSet,
		Dispatcher: disp,
		Facts:      st,
		BufferSize: cfg.BufferSize,
	}, nil
}
