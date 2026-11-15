// Package http wires the service together (constructor DI in one place) and
// runs the public and internal listeners.
package http

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/http/handlers"
	"github.com/boson-chat/files/http/middleware"
	"github.com/boson-chat/files/internal/db"
	filesrepo "github.com/boson-chat/files/internal/db/repositories/files"
	"github.com/boson-chat/files/internal/db/repositories/overrides"
	"github.com/boson-chat/files/internal/leader"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/services/files"
	"github.com/boson-chat/files/internal/services/guest"
	"github.com/boson-chat/files/internal/services/settings"
	"github.com/boson-chat/files/internal/services/stackconfig"
	"github.com/boson-chat/files/internal/services/storage"
	"github.com/boson-chat/files/internal/services/workers"
	"github.com/boson-chat/files/metrics"
)

// Options override dependencies (tests).
type Options struct {
	Metrics        *metrics.Metrics
	MetricsHandler nethttp.Handler
	Now            func() time.Time
	// Storage replaces the driver chosen by the settings.
	Storage storage.Storage
	// TokenVerifier replaces the JWKS verifier.
	TokenVerifier middleware.TokenVerifier
	// Locker replaces the advisory-lock leader election.
	Locker leader.LockerImpl
	// NoNotify turns the config file's fsnotify watch off.
	NoNotify bool
}

// App is the wired service.
type App struct {
	Config      config.Config
	DB          *db.DB
	Files       files.FileServiceImpl
	Settings    settings.SettingsServiceImpl
	StackConfig stackconfig.StackConfigServiceImpl
	Storage     storage.Storage
	Public      nethttp.Handler
	Internal    nethttp.Handler
	Workers     []workers.Worker
	runner      *workers.Runner
}

// NewStorage builds the driver the settings name.
func NewStorage(cfg config.Config, s settings.Settings) (storage.Storage, error) {
	switch s.Storage.Driver {
	case settings.DriverS3:
		return storage.NewS3Storage(storage.S3Options{
			Endpoint: s.Storage.S3.Endpoint, Bucket: s.Storage.S3.Bucket, Region: s.Storage.S3.Region,
			PathStyle: s.Storage.S3.PathStyle, AccessKey: cfg.FilesConfig.S3AccessKey, SecretKey: cfg.FilesConfig.S3SecretKey,
		}), nil
	case settings.DriverLocal, "":
		return storage.NewLocalStorage(cfg.FilesConfig.DataDir)
	}
	return nil, fmt.Errorf("unknown storage driver %q", s.Storage.Driver)
}

// Build constructs everything: settings → database → repositories →
// services → handlers.
func Build(ctx context.Context, cfg config.Config, opts Options) (*App, error) {
	trusted, err := cfg.HTTPConfig.TrustedProxyPrefixes()
	if err != nil {
		return nil, err
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := opts.Metrics
	if m == nil {
		m = metrics.Noop()
	}
	metrics.SetGlobal(m)
	log := logger.FromCtx(ctx)

	// 0. Settings: env, then the config file's files: section.
	envSettings, err := settings.FromEnv(cfg.FilesConfig)
	if err != nil {
		return nil, err
	}
	settingsService := settings.NewSettingsService(envSettings)
	stackConfig := stackconfig.NewStackConfigService(stackconfig.Deps{
		Config: cfg.AppConfig, Settings: settingsService, Now: opts.Now, NoNotify: opts.NoNotify,
	})
	if err := stackConfig.Load(ctx); err != nil {
		return nil, fmt.Errorf("config file %s: %w", cfg.AppConfig.ConfigFile, err)
	}

	// 1. Database
	database, err := db.NewDatabase(ctx, cfg.DBConfig)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*App, error) {
		_ = database.Close()
		return nil, err
	}
	sqlDB, err := database.SQL()
	if err != nil {
		return fail(err)
	}

	// 2. Repositories
	fileRepo := filesrepo.NewFileRepository(database)
	overrideRepo := overrides.NewOverrideRepository(database)

	// 3. Services
	store := opts.Storage
	if store == nil {
		if store, err = NewStorage(cfg, settingsService.Get()); err != nil {
			return fail(err)
		}
	}
	guestService := guest.NewGuestService(guest.Options{
		Secret: cfg.GuestConfig.Secret, TTL: cfg.GuestConfig.TokenTTL, TokensPerHour: cfg.GuestConfig.TokensPerHour, Now: opts.Now,
	})
	if !guestService.Configured() {
		log.Warn().Msg("FILES_GUEST_SECRET is empty: guest uploads are off")
	}
	fileService := files.NewFileService(files.Deps{
		Repo: fileRepo, Overrides: overrideRepo, Tx: database, Storage: store, Settings: settingsService, Metrics: m,
		PublicURL: cfg.HTTPConfig.PublicURL, GuestsConfigured: guestService.Configured(),
		IPQuotaFactor: cfg.GuestConfig.IPQuotaFactor, PendingTimeout: cfg.WorkerConfig.PendingTimeout, Now: opts.Now,
	})
	tokens := opts.TokenVerifier
	if tokens == nil && cfg.AuthConfig.Enabled() {
		v, err := middleware.NewJWTVerifier(ctx, cfg.AuthConfig)
		if err != nil {
			return fail(err)
		}
		tokens = v
	}
	locker := opts.Locker
	if locker == nil {
		locker = leader.NewPGLocker(sqlDB)
	}

	// 4. Handlers
	publicMux := nethttp.NewServeMux()
	(&handlers.Public{
		Files: fileService, Guests: guestService, Settings: settingsService, AccountsEnabled: tokens != nil,
		StorageDriver: store.Driver(), Metrics: m, Now: opts.Now,
	}).Routes(publicMux)
	internalMux := nethttp.NewServeMux()
	(&handlers.Internal{Files: fileService, Token: cfg.InternalConfig.Token, ConfigStatus: stackConfig.Status}).Routes(internalMux)

	ready := func(w nethttp.ResponseWriter, req *nethttp.Request) {
		if err := sqlDB.PingContext(req.Context()); err != nil {
			handlers.WriteJSON(w, nethttp.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		handlers.WriteJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	}
	live := func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		handlers.WriteJSON(w, nethttp.StatusOK, map[string]string{"status": "ok"})
	}
	for _, mux := range []*nethttp.ServeMux{publicMux, internalMux} {
		mux.HandleFunc("GET /healthz", live)
		mux.HandleFunc("GET /readyz", ready)
	}
	if opts.MetricsHandler != nil {
		internalMux.Handle("GET /metrics", opts.MetricsHandler)
	}

	var ws []workers.Worker
	if cfg.WorkerConfig.ExpiryEnabled {
		ws = append(ws, workers.Worker{Name: "expiry", Every: cfg.WorkerConfig.ExpiryEvery, Run: fileService.RunExpiry})
	}

	return &App{
		Config: cfg, DB: database, Files: fileService, Settings: settingsService, StackConfig: stackConfig, Storage: store,
		Public: middleware.Chain(publicMux,
			middleware.Recover, middleware.ClientIP(trusted), middleware.Logging, middleware.SecurityHeaders,
			middleware.CORS, middleware.Authenticate(tokens, guestService, handlers.WriteError)),
		Internal: middleware.Chain(internalMux, middleware.Recover, middleware.ClientIP(nil), middleware.Logging),
		Workers:  ws,
		runner:   workers.NewRunner(locker, m),
	}, nil
}

// RunBackground starts the workers and the config file watcher.
func (a *App) RunBackground(ctx context.Context) {
	a.runner.Start(ctx, a.Workers...)
	go a.StackConfig.Run(ctx)
}

// Close closes the database.
func (a *App) Close() error { return a.DB.Close() }

// Serve runs both listeners until ctx is done, then shuts down gracefully.
// Uploads can be long: the public listener's read timeout is
// FILES_UPLOAD_TIMEOUT and it has no write timeout (large downloads).
func Serve(ctx context.Context, app *App) error {
	cfg := app.Config
	servers := []*nethttp.Server{
		{Addr: cfg.HTTPConfig.Addr, Handler: app.Public, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: cfg.HTTPConfig.UploadTimeout, IdleTimeout: 120 * time.Second},
		{Addr: cfg.InternalConfig.Addr, Handler: app.Internal, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second},
	}
	errCh := make(chan error, len(servers))
	for i, srv := range servers {
		name := []string{"public", "internal"}[i]
		go func() {
			logger.FromCtx(ctx).Info().Str("addr", srv.Addr).Str("listener", name).Msg("listening")
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
				errCh <- fmt.Errorf("%s listener: %w", name, err)
			}
		}()
	}
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}
	return runErr
}
