package commands

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/boson-chat/files/db"
	fileshttp "github.com/boson-chat/files/http"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/tracing"
	"github.com/boson-chat/files/metrics"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the public and internal listeners and the expiry worker",
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		log := logger.FromCtx(ctx)

		if cfg.DBConfig.AutoMigrate {
			if err := db.MigrateUp(cfg.DBConfig.DSN()); err != nil {
				return err
			}
			log.Info().Msg("migrations up to date")
		}
		shutdownTracing, err := tracing.Init(ctx, cfg)
		if err != nil {
			log.Error().Err(err).Msg("tracing disabled")
		} else {
			defer func() {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = shutdownTracing(sctx)
			}()
		}
		mp, err := metrics.NewProvider(ctx, cfg)
		if err != nil {
			return err
		}
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = mp.Shutdown(sctx)
		}()
		m, err := metrics.New(mp.MeterProvider())
		if err != nil {
			return err
		}
		app, err := fileshttp.Build(ctx, cfg, fileshttp.Options{Metrics: m, MetricsHandler: mp.Handler()})
		if err != nil {
			return err
		}
		defer app.Close()
		if !cfg.AuthConfig.Enabled() {
			log.Warn().Msg("AUTH_ISSUER is empty: account uploads are off")
		}
		if cfg.InternalConfig.Token == "" {
			log.Warn().Msg("INTERNAL_TOKEN is empty: the admin API answers 503")
		}
		app.RunBackground(ctx)
		return fileshttp.Serve(ctx, app)
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
}
