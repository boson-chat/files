// Package stackconfig applies the stack config file (BOSON_CONFIG) to the
// running service: its files: section replaces the FILES_* env settings and
// is reloaded live when the file changes. Storage driver settings apply only
// after a restart (listed under restart_required); min_free and capacity
// apply live. Without
// BOSON_CONFIG (or while the file is missing) the env values apply.
package stackconfig

import (
	"context"
	"strings"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/configfile"
	"github.com/boson-chat/files/internal/logger"
	"github.com/boson-chat/files/internal/services/settings"
)

// Deps are the parts the file reconfigures.
type Deps struct {
	Config   config.AppConfig
	Settings settings.SettingsServiceImpl
	Now      func() time.Time
	// NoNotify turns fsnotify off (tests).
	NoNotify bool
}

type StackConfigServiceImpl interface {
	// Load reads the file at start-up.
	Load(ctx context.Context) error
	// Run watches the file until ctx is done (no-op without BOSON_CONFIG).
	Run(ctx context.Context)
	// Check re-reads the file now.
	Check(ctx context.Context)
	// Status is GET /internal/config/status.
	Status() configfile.Status
}

type StackConfigService struct {
	d Deps
	w *configfile.Watcher[settings.Settings]
}

func NewStackConfigService(d Deps) *StackConfigService {
	s := &StackConfigService{d: d}
	if d.Config.ConfigFile == "" {
		return s
	}
	log := logger.FromCtx(context.Background()).With().Str("component", "config").Logger()
	env := d.Settings.Env()
	effective := func(st configfile.State[settings.Settings]) settings.Settings {
		if st.Present {
			return st.Value
		}
		return env
	}
	s.w = configfile.New(configfile.Options[settings.Settings]{
		Path: d.Config.ConfigFile,
		Load: func(doc *configfile.Document) (settings.Settings, error) {
			return settings.LoadFile(doc, env)
		},
		Apply: s.apply,
		// The storage driver is built once at start-up.
		RestartRequired: func(boot, cur configfile.State[settings.Settings]) []string {
			var out []string
			b, c := effective(boot).Flatten(), effective(cur).Flatten()
			for _, k := range configfile.ChangedKeys(b, c) {
				if strings.HasPrefix(k, "storage.") && k != "storage.min_free" && k != "storage.capacity" {
					out = append(out, settings.Section+"."+k)
				}
			}
			return out
		},
		Poll:     d.Config.ConfigPoll,
		Log:      &log,
		Now:      d.Now,
		NoNotify: d.NoNotify,
	})
	return s
}

func (s *StackConfigService) apply(ctx context.Context, c configfile.Change[settings.Settings]) {
	log := logger.FromCtx(ctx)
	if c.Next.Present {
		v := c.Next.Value
		if len(v.Unknown) > 0 {
			log.Warn().Strs("keys", v.Unknown).Msg("config file: ignoring unknown keys")
		}
		s.d.Settings.SetFile(&v)
	} else {
		s.d.Settings.SetFile(nil)
	}
	if c.Initial {
		return
	}
	var before, after map[string]any
	if c.Prev.Present {
		before = c.Prev.Value.Flatten()
	}
	if c.Next.Present {
		after = c.Next.Value.Flatten()
	}
	if changed := configfile.ChangedKeys(before, after); len(changed) > 0 {
		log.Info().Strs("changed", changed).Bool("present", c.Next.Present).Msg("files config reloaded")
	}
}

func (s *StackConfigService) Load(ctx context.Context) error {
	if s.w == nil {
		return nil
	}
	return s.w.Load(ctx)
}

func (s *StackConfigService) Run(ctx context.Context) {
	if s.w != nil {
		s.w.Run(ctx)
	}
}

func (s *StackConfigService) Check(ctx context.Context) {
	if s.w != nil {
		s.w.Check(ctx)
	}
}

func (s *StackConfigService) Status() configfile.Status {
	if s.w == nil {
		return configfile.Status{OK: true, RestartRequired: []string{}}
	}
	return s.w.Status()
}
