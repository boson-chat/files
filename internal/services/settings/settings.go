// Package settings holds the effective upload policy: the FILES_* env
// defaults overridden by the files: section of the stack config file
// (BOSON_CONFIG). A key set in the file wins; an absent key falls back to
// env, then the built-in default; removing a key resets it.
//
//	files:
//	  enabled: true
//	  retention: 72h
//	  window: 72h
//	  account: { quota: 5GB, max_file: 100MB }
//	  guest:   { enabled: true, quota: 500MB, max_file: 25MB }
//	  storage:
//	    driver: local            # local | s3
//	    min_free: 2GB            # refuse uploads below this much free space
//	    capacity: 15GB           # s3 only: total budget (optional)
//	    s3: { endpoint: "", bucket: "", region: auto, path_style: true, public_base_url: "" }
//
// S3 keys come only from FILES_S3_ACCESS_KEY / FILES_S3_SECRET_KEY.
package settings

import (
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/configfile"
)

// Section is the config file section this service reads.
const Section = "files"

// Storage drivers.
const (
	DriverLocal = "local"
	DriverS3    = "s3"
)

// Limits are one class's limits (accounts or guests).
type Limits struct {
	QuotaBytes   int64 `json:"quota_bytes"`
	MaxFileBytes int64 `json:"max_file_bytes"`
}

// S3 is the s3 driver's settings (keys excluded).
type S3 struct {
	Endpoint      string `json:"endpoint"`
	Bucket        string `json:"bucket"`
	Region        string `json:"region"`
	PathStyle     bool   `json:"path_style"`
	PublicBaseURL string `json:"public_base_url"`
}

// Storage is where file bodies go.
type Storage struct {
	Driver string `json:"driver"`
	S3     S3     `json:"s3"`
	// MinFree: new uploads are refused (507 storage_full) when they would
	// leave less free space than this. Local driver: the volume's free
	// space (statfs). s3: Capacity minus the stored bytes, when Capacity
	// is set (S3 has no free-space API).
	MinFree int64 `json:"min_free_bytes"`
	// Capacity is the s3 driver's total byte budget (0 = unlimited, no
	// guard). Ignored by the local driver.
	Capacity int64 `json:"capacity_bytes"`
}

// Settings are the effective files settings.
type Settings struct {
	Enabled      bool          `json:"enabled"`
	Retention    time.Duration `json:"-"`
	Window       time.Duration `json:"-"`
	Account      Limits        `json:"account"`
	GuestEnabled bool          `json:"guest_enabled"`
	Guest        Limits        `json:"guest"`
	Storage      Storage       `json:"storage"`
	// Unknown lists keys of files: this version doesn't know (logged).
	Unknown []string `json:"-"`
}

// FromEnv resolves the settings from env values and defaults (no file).
func FromEnv(c config.FilesConfig) (Settings, error) {
	var s Settings
	var err error
	size := func(name, v string) int64 {
		if err != nil {
			return 0
		}
		var n int64
		if n, err = config.ParseSize(v); err == nil && n <= 0 {
			err = fmt.Errorf("%s must be positive", name)
		} else if err != nil {
			err = fmt.Errorf("%s: %w", name, err)
		}
		return n
	}
	sizeOrZero := func(name, v string) int64 {
		if err != nil || strings.TrimSpace(v) == "" {
			return 0
		}
		var n int64
		if n, err = config.ParseSize(v); err != nil {
			err = fmt.Errorf("%s: %w", name, err)
		}
		return n
	}
	s = Settings{
		Enabled:      c.Enabled,
		Retention:    c.Retention,
		Window:       c.Window,
		Account:      Limits{QuotaBytes: size("FILES_ACCOUNT_QUOTA", c.AccountQuota), MaxFileBytes: size("FILES_ACCOUNT_MAX_FILE", c.AccountMax)},
		GuestEnabled: c.GuestEnabled,
		Guest:        Limits{QuotaBytes: size("FILES_GUEST_QUOTA", c.GuestQuota), MaxFileBytes: size("FILES_GUEST_MAX_FILE", c.GuestMax)},
		Storage: Storage{
			Driver:   strings.ToLower(strings.TrimSpace(c.StorageDriver)),
			MinFree:  sizeOrZero("FILES_STORAGE_MIN_FREE", c.StorageMinFree),
			Capacity: sizeOrZero("FILES_STORAGE_CAPACITY", c.StorageCapacity),
			S3: S3{
				Endpoint: c.S3Endpoint, Bucket: c.S3Bucket, Region: c.S3Region, PathStyle: c.S3PathStyle,
				PublicBaseURL: strings.TrimRight(c.S3PublicBaseURL, "/"),
			},
		},
	}
	if err != nil {
		return s, err
	}
	return s, s.Validate()
}

// Validate checks the rules the file also applies.
func (s Settings) Validate() error {
	if s.Retention <= 0 || s.Window <= 0 {
		return fmt.Errorf("files retention and window must be positive")
	}
	if s.Account.QuotaBytes <= 0 || s.Account.MaxFileBytes <= 0 || s.Guest.QuotaBytes <= 0 || s.Guest.MaxFileBytes <= 0 {
		return fmt.Errorf("files quotas and max_file sizes must be positive")
	}
	switch s.Storage.Driver {
	case DriverLocal:
	case DriverS3:
		if s.Storage.S3.Bucket == "" {
			return fmt.Errorf("files storage driver s3 needs a bucket (FILES_S3_BUCKET or files.storage.s3.bucket)")
		}
	default:
		return fmt.Errorf("files storage driver must be local or s3, got %q", s.Storage.Driver)
	}
	return nil
}

// LoadFile decodes and validates the files: section over base (the env
// settings). Errors carry the line and column of the value.
func LoadFile(doc *configfile.Document, base Settings) (Settings, error) {
	out := base
	out.Unknown = nil
	sec, err := doc.Section(Section)
	if err != nil {
		return out, err
	}
	acc, err := sec.Sub("account")
	if err != nil {
		return out, err
	}
	gst, err := sec.Sub("guest")
	if err != nil {
		return out, err
	}
	st, err := sec.Sub("storage")
	if err != nil {
		return out, err
	}
	s3, err := st.Sub("s3")
	if err != nil {
		return out, err
	}
	out.Unknown = append(out.Unknown, sec.Unknown("enabled", "retention", "window", "account", "guest", "storage")...)
	out.Unknown = append(out.Unknown, acc.Unknown("quota", "max_file")...)
	out.Unknown = append(out.Unknown, gst.Unknown("enabled", "quota", "max_file")...)
	out.Unknown = append(out.Unknown, st.Unknown("driver", "min_free", "capacity", "s3")...)
	for _, k := range []string{"access_key", "secret_key", "access_key_id", "secret_access_key"} {
		if s3.Has(k) {
			return out, s3.Errorf(k, "doesn't belong in the config file; set FILES_S3_ACCESS_KEY / FILES_S3_SECRET_KEY in .env")
		}
	}
	out.Unknown = append(out.Unknown, s3.Unknown("endpoint", "bucket", "region", "path_style", "public_base_url")...)

	setBool := func(sec *configfile.Section, key string, dst *bool) error {
		v, err := sec.Bool(key)
		if err == nil && v != nil {
			*dst = *v
		}
		return err
	}
	setDur := func(key string, dst *time.Duration) error {
		v, err := sec.Duration(key)
		if err != nil || v == nil {
			return err
		}
		if *v <= 0 {
			return sec.Errorf(key, "must be positive")
		}
		*dst = *v
		return nil
	}
	setSize0 := func(sec *configfile.Section, key string, dst *int64) error {
		raw, err := sec.String(key)
		if err != nil || raw == nil {
			return err
		}
		v, err := config.ParseSize(*raw)
		if err != nil {
			return sec.Errorf(key, "%s", err.Error())
		}
		*dst = v
		return nil
	}
	setSize := func(sec *configfile.Section, key string, dst *int64) error {
		raw, err := sec.String(key)
		if err != nil || raw == nil {
			return err
		}
		v, err := config.ParseSize(*raw)
		if err != nil {
			return sec.Errorf(key, "%s", err.Error())
		}
		if v <= 0 {
			return sec.Errorf(key, "must be positive")
		}
		*dst = v
		return nil
	}
	setStr := func(sec *configfile.Section, key string, dst *string) error {
		v, err := sec.String(key)
		if err == nil && v != nil {
			*dst = strings.TrimSpace(*v)
		}
		return err
	}
	steps := []func() error{
		func() error { return setBool(sec, "enabled", &out.Enabled) },
		func() error { return setDur("retention", &out.Retention) },
		func() error { return setDur("window", &out.Window) },
		func() error { return setSize(acc, "quota", &out.Account.QuotaBytes) },
		func() error { return setSize(acc, "max_file", &out.Account.MaxFileBytes) },
		func() error { return setBool(gst, "enabled", &out.GuestEnabled) },
		func() error { return setSize(gst, "quota", &out.Guest.QuotaBytes) },
		func() error { return setSize(gst, "max_file", &out.Guest.MaxFileBytes) },
		func() error {
			if err := setStr(st, "driver", &out.Storage.Driver); err != nil {
				return err
			}
			out.Storage.Driver = strings.ToLower(out.Storage.Driver)
			if d := out.Storage.Driver; d != DriverLocal && d != DriverS3 {
				return st.Errorf("driver", "must be local or s3")
			}
			return nil
		},
		func() error { return setSize0(st, "min_free", &out.Storage.MinFree) },
		func() error { return setSize0(st, "capacity", &out.Storage.Capacity) },
		func() error { return setStr(s3, "endpoint", &out.Storage.S3.Endpoint) },
		func() error { return setStr(s3, "bucket", &out.Storage.S3.Bucket) },
		func() error { return setStr(s3, "region", &out.Storage.S3.Region) },
		func() error { return setBool(s3, "path_style", &out.Storage.S3.PathStyle) },
		func() error {
			if err := setStr(s3, "public_base_url", &out.Storage.S3.PublicBaseURL); err != nil {
				return err
			}
			out.Storage.S3.PublicBaseURL = strings.TrimRight(out.Storage.S3.PublicBaseURL, "/")
			if u := out.Storage.S3.PublicBaseURL; u != "" {
				if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
					return s3.Errorf("public_base_url", "must be an http(s) URL")
				}
			}
			return nil
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return out, err
		}
	}
	if out.Storage.Driver == DriverS3 && out.Storage.S3.Bucket == "" {
		if st.Has("driver") {
			return out, st.Errorf("driver", "s3 needs files.storage.s3.bucket (or FILES_S3_BUCKET)")
		}
		return out, fmt.Errorf("files.storage: s3 needs a bucket")
	}
	if out.Storage.S3.Region == "" {
		out.Storage.S3.Region = "auto"
	}
	return out, out.Validate()
}

// Flatten is a key → value map for change detection and the status echo.
func (s Settings) Flatten() map[string]any {
	return map[string]any{
		"enabled": s.Enabled, "retention": s.Retention.String(), "window": s.Window.String(),
		"account.quota": s.Account.QuotaBytes, "account.max_file": s.Account.MaxFileBytes,
		"guest.enabled": s.GuestEnabled, "guest.quota": s.Guest.QuotaBytes, "guest.max_file": s.Guest.MaxFileBytes,
		"storage.driver": s.Storage.Driver, "storage.min_free": s.Storage.MinFree, "storage.capacity": s.Storage.Capacity,
		"storage.s3.endpoint": s.Storage.S3.Endpoint,
		"storage.s3.bucket":   s.Storage.S3.Bucket, "storage.s3.region": s.Storage.S3.Region,
		"storage.s3.path_style": s.Storage.S3.PathStyle, "storage.s3.public_base_url": s.Storage.S3.PublicBaseURL,
	}
}

// Echo is the config echo in GET /internal/files/status.
type Echo struct {
	Enabled          bool    `json:"enabled"`
	RetentionSeconds int64   `json:"retention_seconds"`
	WindowSeconds    int64   `json:"window_seconds"`
	Account          Limits  `json:"account"`
	Guest            Limits  `json:"guest"`
	GuestEnabled     bool    `json:"guest_enabled"`
	Storage          Storage `json:"storage"`
	// Source is "file" when the config file's files: section is in use,
	// else "env".
	Source string `json:"source"`
}

// SettingsServiceImpl holds the settings in use; safe for concurrent use.
type SettingsServiceImpl interface {
	Get() Settings
	// SetFile makes file-derived settings live (nil: back to env).
	SetFile(s *Settings)
	// Env is the env-only settings (the base the file overlays).
	Env() Settings
	// Source is "file" or "env".
	Source() string
	Echo() Echo
}

type SettingsService struct {
	mu   sync.RWMutex
	env  Settings
	file *Settings
}

// NewSettingsService starts on env settings.
func NewSettingsService(env Settings) *SettingsService { return &SettingsService{env: env} }

func (s *SettingsService) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file != nil {
		return *s.file
	}
	return s.env
}

func (s *SettingsService) Env() Settings { return s.env }

func (s *SettingsService) SetFile(v *Settings) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v == nil {
		s.file = nil
		return
	}
	c := *v
	s.file = &c
}

func (s *SettingsService) Source() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file != nil {
		return "file"
	}
	return "env"
}

func (s *SettingsService) Echo() Echo {
	v := s.Get()
	return Echo{
		Enabled: v.Enabled, RetentionSeconds: int64(v.Retention / time.Second), WindowSeconds: int64(v.Window / time.Second),
		Account: v.Account, Guest: v.Guest, GuestEnabled: v.GuestEnabled, Storage: v.Storage, Source: s.Source(),
	}
}
