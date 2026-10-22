// Command herdr-vitals is the Herdr plugin process: launched once by a
// startup hook, it stays alive and keeps each Claude pane's context usage
// published as a sidebar token.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/mcditchman/herdr-vitals/internal/app"
	"github.com/mcditchman/herdr-vitals/internal/herdr"
	"github.com/mcditchman/herdr-vitals/internal/transcript"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(log); err != nil {
		log.Error("vitals stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, warnings := app.LoadConfig(os.Getenv("HERDR_PLUGIN_CONFIG_DIR"))
	for _, warning := range warnings {
		log.Warn(warning)
	}

	log.Info("starting vitals", "poll", cfg.Poll, "token", cfg.Token, "bar_width", cfg.BarWidth)

	client, err := herdr.New()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app.New(cfg, log, client, transcript.NewReader(cfg.ClaudeDir)).Run(ctx)

	return nil
}
