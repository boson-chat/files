package settings

import (
	"strings"
	"testing"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/configfile"
)

func envDefaults() config.FilesConfig {
	return config.FilesConfig{
		Enabled: true, Retention: 72 * time.Hour, Window: 72 * time.Hour,
		AccountQuota: "5GB", AccountMax: "100MB", GuestEnabled: true, GuestQuota: "500MB", GuestMax: "25MB",
		StorageDriver: "local", StorageMinFree: "2GB", S3Region: "auto", S3PathStyle: true,
	}
}

func TestFromEnvDefaults(t *testing.T) {
	s, err := FromEnv(envDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if s.Account.QuotaBytes != 5e9 || s.Account.MaxFileBytes != 1e8 || s.Guest.QuotaBytes != 5e8 || s.Guest.MaxFileBytes != 25e6 ||
		s.Storage.MinFree != 2e9 || s.Storage.Capacity != 0 || s.Storage.Driver != DriverLocal || s.Window != 72*time.Hour {
		t.Fatalf("%+v", s)
	}
	bad := envDefaults()
	bad.StorageDriver = "s3"
	if _, err := FromEnv(bad); err == nil {
		t.Fatal("s3 without bucket accepted")
	}
}

func load(t *testing.T, src string) (Settings, error) {
	t.Helper()
	base, err := FromEnv(envDefaults())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := configfile.Parse([]byte(src))
	if err != nil {
		return Settings{}, err
	}
	return LoadFile(doc, base)
}

func TestLoadFileContractExample(t *testing.T) {
	s, err := load(t, `version: 1
files:
  enabled: true
  retention: 48h
  window: 24h
  account: { quota: 10GB, max_file: 200MB }
  guest:   { enabled: false, quota: 1GB, max_file: 10MB }
  storage:
    driver: s3
    min_free: 1GB
    capacity: 15GB
    s3: { endpoint: "http://minio:9000", bucket: "files", region: auto, path_style: true, public_base_url: "" }
  typo: 1
other: { x: 1 }
`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Retention != 48*time.Hour || s.Window != 24*time.Hour || s.Account.QuotaBytes != 10e9 || s.Account.MaxFileBytes != 200e6 ||
		s.GuestEnabled || s.Guest.QuotaBytes != 1e9 || s.Guest.MaxFileBytes != 10e6 ||
		s.Storage.Driver != DriverS3 || s.Storage.S3.Bucket != "files" || s.Storage.S3.Endpoint != "http://minio:9000" ||
		s.Storage.MinFree != 1e9 || s.Storage.Capacity != 15e9 {
		t.Fatalf("%+v", s)
	}
	if len(s.Unknown) != 1 || s.Unknown[0] != "files.typo" {
		t.Fatal(s.Unknown)
	}
}

func TestLoadFileAbsentKeysFallBack(t *testing.T) {
	s, err := load(t, "files:\n  account: { quota: 1GiB }\n")
	if err != nil {
		t.Fatal(err)
	}
	if s.Account.QuotaBytes != 1<<30 || s.Account.MaxFileBytes != 1e8 || s.Window != 72*time.Hour || !s.Enabled {
		t.Fatalf("%+v", s)
	}
	// No files: section at all = env.
	s, err = load(t, "version: 1\nauth: {}\n")
	if err != nil || s.Account.QuotaBytes != 5e9 {
		t.Fatal(s, err)
	}
	// Plain byte counts.
	s, err = load(t, "files:\n  guest: { max_file: 1048576 }\n")
	if err != nil || s.Guest.MaxFileBytes != 1048576 {
		t.Fatal(s, err)
	}
}

func TestLoadFileErrorsArePositioned(t *testing.T) {
	cases := map[string]struct {
		src  string
		line int
		want string
	}{
		"bad size":     {"files:\n  account:\n    quota: lots\n", 3, "invalid size"},
		"zero":         {"files:\n  account: { max_file: 0 }\n", 2, "positive"},
		"bad duration": {"files:\n  window: 3 days\n", 2, "duration"},
		"bad driver":   {"files:\n  storage: { driver: ftp }\n", 2, "local or s3"},
		"secret":       {"files:\n  storage:\n    s3: { bucket: b, secret_key: x }\n", 3, "FILES_S3_SECRET_KEY"},
		"s3 no bucket": {"files:\n  storage:\n    driver: s3\n", 3, "bucket"},
		"bool":         {"files:\n  enabled: maybe\n", 2, "true or false"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, c.src)
			e, ok := configfile.AsError(err)
			if !ok || e.Line != c.line || !strings.Contains(e.Msg, c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestServiceFileOverlay(t *testing.T) {
	env, _ := FromEnv(envDefaults())
	svc := NewSettingsService(env)
	if svc.Source() != "env" {
		t.Fatal()
	}
	f := env
	f.Enabled = false
	svc.SetFile(&f)
	if svc.Get().Enabled || svc.Source() != "file" || svc.Echo().Source != "file" {
		t.Fatal("file not applied")
	}
	svc.SetFile(nil)
	if !svc.Get().Enabled {
		t.Fatal("env not restored")
	}
	if e := svc.Echo(); e.WindowSeconds != 259200 || e.RetentionSeconds != 259200 {
		t.Fatalf("%+v", e)
	}
}
