// Package commands holds the cobra CLI: serve, migrate.
package commands

import (
	"os"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/logger"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:          "files",
	Short:        "boson file uploads with a rolling per-user quota",
	SilenceUsage: true,
}

// Execute runs the root command and exits non-zero on error.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func loadConfig() (config.Config, error) {
	cfg, err := config.LoadConfig()
	if err != nil {
		return cfg, err
	}
	logger.Logger(
		logger.WithServerName(cfg.AppConfig.APPName),
		logger.WithVersion(cfg.AppConfig.Version),
		logger.WithEnvironment(cfg.AppConfig.Env),
		logger.WithLevel(cfg.AppConfig.LogLevel),
	)
	return cfg, nil
}
