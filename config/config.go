// Package config holds the service configuration, loaded from env vars.
//
// The upload policy (enabled, retention, window, limits, storage) has env
// defaults here and is overridden by the files: section of the stack config
// file (BOSON_CONFIG), see internal/services/settings.
//
// Durations use Go syntax (15m, 72h). Sizes accept a plain byte count or a
// number with a unit: KB/MB/GB/TB are powers of 1000, KiB/MiB/GiB/TiB powers
// of 1024.
package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/jinzhu/configor"
)

type Config struct {
	AppConfig       AppConfig
	HTTPConfig      HTTPConfig
	InternalConfig  InternalConfig
	DBConfig        DBConfig
	AuthConfig      AuthConfig
	GuestConfig     GuestConfig
	FilesConfig     FilesConfig
	WorkerConfig    WorkerConfig
	TelemetryConfig TelemetryConfig
}

type AppConfig struct {
	APPName  string `default:"files"`
	Env      string `default:"development" env:"ENV"`
	Version  string `default:"dev" env:"VERSION"`
	LogLevel string `default:"info" env:"LOG_LEVEL"`
	// Stack config file (config/boson.yaml). Its files: section wins over
	// the FILES_* env values and is reloaded live.
	ConfigFile string `env:"BOSON_CONFIG"`
	// How often the config file's mtime is polled (besides fsnotify).
	ConfigPoll time.Duration `default:"10s" env:"BOSON_CONFIG_POLL"`
}

// IsProduction reports ENV=production (the image default).
func (a AppConfig) IsProduction() bool { return strings.EqualFold(a.Env, "production") }

type HTTPConfig struct {
	// Public listener (files.<domain>, behind the reverse proxy).
	Addr string `default:":8080" env:"HTTP_ADDR"`
	// Externally visible base URL, e.g. https://files.boson.chat. File URLs
	// are built from it.
	PublicURL string `env:"FILES_PUBLIC_URL"`
	// Comma-separated CIDRs (or IPs) whose X-Forwarded-For is trusted.
	TrustedProxies string `env:"TRUSTED_PROXIES"`
	// Upper bound for reading one request (an upload) from the client.
	UploadTimeout time.Duration `default:"30m" env:"FILES_UPLOAD_TIMEOUT"`
	// Pages allowed to call the API (uploads, usage, guest tokens, own
	// files) from a browser (CORS): comma-separated origins, "*" = any, or
	// "directory" = the origins at ORIGINS_URL (mixable with fixed
	// origins, which stay allowed when that list can't be fetched).
	// Downloads (/f/…), file metadata and GET /v1/config stay open to any
	// page. Requests carry bearer/guest tokens, never cookies.
	CORSAllowedOrigins string `default:"*" env:"CORS_ALLOWED_ORIGINS"`
	// The web origins list for "directory": the auth service's
	// GET /v1/web-origins ({"origins": [...]}), e.g.
	// http://auth:8080/v1/web-origins. Fetched at start and every
	// OriginsRefresh; the last good list stays when it fails.
	OriginsURL     string        `env:"ORIGINS_URL"`
	OriginsRefresh time.Duration `default:"5m" env:"ORIGINS_REFRESH"`
}

// UsesDirectory reports whether CORS_ALLOWED_ORIGINS has "directory".
func (c HTTPConfig) UsesDirectory() bool {
	for _, e := range strings.Split(c.CORSAllowedOrigins, ",") {
		if strings.EqualFold(strings.TrimSpace(e), "directory") {
			return true
		}
	}
	return false
}

// InternalConfig is the internal listener: /internal/*, /metrics, health.
// Compose network only; never publish its port.
type InternalConfig struct {
	Addr string `default:":3001" env:"INTERNAL_ADDR"`
	// Shared secret the admin panel sends as X-Internal-Token on
	// /internal/files*. Empty: those endpoints answer 503.
	Token string `env:"INTERNAL_TOKEN"`
}

type DBConfig struct {
	// Full connection URL; when set it wins over the DB_* parts.
	URL      string `env:"DATABASE_URL"`
	Host     string `default:"localhost" env:"DB_HOST"`
	Port     uint   `default:"5432" env:"DB_PORT"`
	User     string `default:"files" env:"DB_USER"`
	Password string `env:"DB_PASSWORD"`
	Name     string `default:"files" env:"DB_NAME"`
	SSLMode  string `default:"disable" env:"DB_SSLMODE"`
	// Run migrations up on `serve` start.
	AutoMigrate bool `default:"true" env:"AUTO_MIGRATE"`
	MaxConns    int  `default:"20" env:"DB_MAX_CONNS"`
}

// DSN returns the connection URL.
func (c DBConfig) DSN() string {
	if c.URL != "" {
		return c.URL
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(c.User, c.Password),
		Host:     fmt.Sprintf("%s:%d", c.Host, c.Port),
		Path:     "/" + c.Name,
		RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
	}
	return u.String()
}

// AuthConfig points at the boson auth service whose access tokens identify
// accounts. Empty AUTH_ISSUER turns account uploads off.
type AuthConfig struct {
	Issuer string `env:"AUTH_ISSUER"`
	// Defaults to AUTH_ISSUER + /.well-known/jwks.json.
	JWKSURL  string `env:"AUTH_JWKS_URL"`
	Audience string `default:"boson" env:"AUTH_AUDIENCE"`
}

// Enabled reports whether account (bearer) auth is configured.
func (a AuthConfig) Enabled() bool { return a.Issuer != "" }

// GuestConfig: guest tokens (POST /v1/guest-token).
type GuestConfig struct {
	// HMAC key for guest tokens and the hashed IP keys. Empty: guest
	// uploads are off whatever the config file says.
	Secret   string        `env:"FILES_GUEST_SECRET"`
	TokenTTL time.Duration `default:"24h" env:"FILES_GUEST_TOKEN_TTL"`
	// Guest tokens one client IP may get per hour (burst = the same).
	TokensPerHour int `default:"10" env:"FILES_GUEST_TOKENS_PER_HOUR"`
	// Multiplier of the guest quota for the per-IP total.
	IPQuotaFactor int `default:"3" env:"FILES_GUEST_IP_FACTOR"`
}

// FilesConfig are the env defaults of the files: section of the config
// file. A key set in the file wins; an absent key falls back to these.
type FilesConfig struct {
	Enabled       bool          `default:"true" env:"FILES_ENABLED"`
	Retention     time.Duration `default:"72h" env:"FILES_RETENTION"`
	Window        time.Duration `default:"72h" env:"FILES_WINDOW"`
	AccountQuota  string        `default:"5GB" env:"FILES_ACCOUNT_QUOTA"`
	AccountMax    string        `default:"100MB" env:"FILES_ACCOUNT_MAX_FILE"`
	GuestEnabled  bool          `default:"true" env:"FILES_GUEST_ENABLED"`
	GuestQuota    string        `default:"500MB" env:"FILES_GUEST_QUOTA"`
	GuestMax      string        `default:"25MB" env:"FILES_GUEST_MAX_FILE"`
	StorageDriver string        `default:"local" env:"FILES_STORAGE_DRIVER"`
	// Uploads are refused (507 storage_full) below this much free space.
	StorageMinFree string `default:"2GB" env:"FILES_STORAGE_MIN_FREE"`
	// s3 only: total byte budget checked against the stored bytes (empty =
	// no guard; S3 has no free-space API).
	StorageCapacity string `env:"FILES_STORAGE_CAPACITY"`
	// local driver: where files are kept (/data in the image).
	DataDir         string `default:"./data/files" env:"FILES_DATA_DIR"`
	S3Endpoint      string `env:"FILES_S3_ENDPOINT"`
	S3Bucket        string `env:"FILES_S3_BUCKET"`
	S3Region        string `default:"auto" env:"FILES_S3_REGION"`
	S3PathStyle     bool   `default:"true" env:"FILES_S3_PATH_STYLE"`
	S3PublicBaseURL string `env:"FILES_S3_PUBLIC_BASE_URL"`
	// Keys never go in the config file.
	S3AccessKey string `env:"FILES_S3_ACCESS_KEY"`
	S3SecretKey string `env:"FILES_S3_SECRET_KEY"`
}

type WorkerConfig struct {
	// Expiry worker: deletes expired files from storage, retries failed
	// deletes, prunes ledger rows that left the quota window and reclaims
	// stale reservations.
	ExpiryEnabled bool          `default:"true" env:"FILES_EXPIRY_ENABLED"`
	ExpiryEvery   time.Duration `default:"1m" env:"FILES_EXPIRY_EVERY"`
	// A reservation (upload in progress) older than this is released.
	PendingTimeout time.Duration `default:"1h" env:"FILES_PENDING_TIMEOUT"`
}

// TelemetryConfig controls metrics/trace export. Prometheus on the internal
// listener's /metrics is always on; OTLP push is on when an endpoint is set.
type TelemetryConfig struct {
	OTLPEndpoint        string `env:"OTEL_EXPORTER_OTLP_ENDPOINT"`
	OTLPMetricsEndpoint string `env:"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"`
	OTLPTracesEndpoint  string `env:"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"`
	OTLPProtocol        string `env:"OTEL_EXPORTER_OTLP_PROTOCOL"`
	OTLPMetricsProtocol string `env:"OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"`
	MetricsExporter     string `env:"OTEL_METRICS_EXPORTER"`
	TracesExporter      string `env:"OTEL_TRACES_EXPORTER"`
}

// LoadConfig reads the configuration from the environment and fills derived defaults.
func LoadConfig() (Config, error) {
	var cfg Config
	if err := configor.New(&configor.Config{ENVPrefix: "-", Silent: true}).Load(&cfg); err != nil {
		return cfg, err
	}
	cfg.ApplyDefaults()
	return cfg, cfg.Validate()
}

// ApplyDefaults fills values derived from other settings.
func (c *Config) ApplyDefaults() {
	if c.HTTPConfig.PublicURL == "" {
		c.HTTPConfig.PublicURL = "http://localhost:8080"
	}
	c.HTTPConfig.PublicURL = strings.TrimRight(c.HTTPConfig.PublicURL, "/")
	c.AuthConfig.Issuer = strings.TrimRight(c.AuthConfig.Issuer, "/")
	if c.AuthConfig.JWKSURL == "" && c.AuthConfig.Issuer != "" {
		c.AuthConfig.JWKSURL = c.AuthConfig.Issuer + "/.well-known/jwks.json"
	}
	if c.GuestConfig.TokenTTL <= 0 {
		c.GuestConfig.TokenTTL = 24 * time.Hour
	}
	if c.GuestConfig.IPQuotaFactor <= 0 {
		c.GuestConfig.IPQuotaFactor = 3
	}
}

// Validate checks settings that would otherwise fail confusingly later.
func (c Config) Validate() error {
	if c.HTTPConfig.UsesDirectory() && strings.TrimSpace(c.HTTPConfig.OriginsURL) == "" {
		return errors.New("CORS_ALLOWED_ORIGINS says directory: set ORIGINS_URL (the auth service's /v1/web-origins, e.g. http://auth:8080/v1/web-origins)")
	}
	if _, err := c.HTTPConfig.TrustedProxyPrefixes(); err != nil {
		return err
	}
	if c.WorkerConfig.ExpiryEvery <= 0 || c.WorkerConfig.PendingTimeout <= 0 {
		return errors.New("FILES_EXPIRY_EVERY and FILES_PENDING_TIMEOUT must be positive")
	}
	if c.GuestConfig.Secret != "" && len(c.GuestConfig.Secret) < 16 {
		return errors.New("FILES_GUEST_SECRET must be at least 16 characters")
	}
	for name, v := range map[string]string{
		"FILES_ACCOUNT_QUOTA": c.FilesConfig.AccountQuota, "FILES_ACCOUNT_MAX_FILE": c.FilesConfig.AccountMax,
		"FILES_GUEST_QUOTA": c.FilesConfig.GuestQuota, "FILES_GUEST_MAX_FILE": c.FilesConfig.GuestMax,
	} {
		if n, err := ParseSize(v); err != nil || n <= 0 {
			return fmt.Errorf("%s: %q is not a positive size", name, v)
		}
	}
	return nil
}

// TrustedProxyPrefixes parses TRUSTED_PROXIES.
func (c HTTPConfig) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range SplitList(c.TrustedProxies) {
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES: %q is not an IP or CIDR", s)
		}
		out = append(out, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
	}
	return out, nil
}

// SplitList splits a comma/space-separated env value.
func SplitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}
