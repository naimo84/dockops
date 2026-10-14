// Command dockops is the entry point of the dockops agent.
//
// Phase 1 scope: load the declarative configuration (root stacks.yaml plus
// all discovered stack definitions) and report the number of stacks.
package main

import (
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/local/dockops/internal/config"
)

func main() {
	var configFile string
	flag.StringVar(&configFile, "config", "stacks.yaml", "path to the root stacks.yaml configuration file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := config.Load(configFile)
	if err != nil {
		logger.Error("loading configuration failed", "config", configFile, "error", err)
		os.Exit(1)
	}

	enabled := 0
	for _, stack := range cfg.Stacks {
		if stack.Enabled {
			enabled++
		}
	}
	logger.Info("configuration loaded",
		"stacks", len(cfg.Stacks),
		"enabled", enabled,
		"poll_interval", time.Duration(cfg.Global.PollInterval).String(),
	)
}
