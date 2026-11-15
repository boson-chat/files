package stackconfig

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/services/settings"
)

func TestReloadAppliesLiveAndFlagsStorage(t *testing.T) {
	env, err := settings.FromEnv(config.FilesConfig{
		Enabled: true, Retention: 72 * time.Hour, Window: 72 * time.Hour, AccountQuota: "5GB", AccountMax: "100MB",
		GuestEnabled: true, GuestQuota: "500MB", GuestMax: "25MB", StorageDriver: "local", StorageMinFree: "2GB",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "boson.yaml")
	if err := os.WriteFile(path, []byte("files:\n  account: { quota: 1GB }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := settings.NewSettingsService(env)
	sc := NewStackConfigService(Deps{Config: config.AppConfig{ConfigFile: path, ConfigPoll: time.Hour}, Settings: svc, NoNotify: true})
	ctx := context.Background()
	if err := sc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if svc.Get().Account.QuotaBytes != 1e9 {
		t.Fatal("file not applied at start")
	}

	// Limits and min_free reload live; the driver needs a restart.
	write := func(src string) {
		tmp := path + ".tmp"
		_ = os.WriteFile(tmp, []byte(src), 0o644)
		_ = os.Rename(tmp, path)
	}
	write("files:\n  account: { quota: 2GB }\n  storage: { min_free: 5GB }\n")
	sc.Check(ctx)
	if svc.Get().Account.QuotaBytes != 2e9 || svc.Get().Storage.MinFree != 5e9 {
		t.Fatalf("%+v", svc.Get())
	}
	if st := sc.Status(); !st.OK || len(st.RestartRequired) != 0 {
		t.Fatalf("%+v", st)
	}
	write("files:\n  storage: { driver: s3, s3: { bucket: b } }\n")
	sc.Check(ctx)
	st := sc.Status()
	if len(st.RestartRequired) == 0 || st.RestartRequired[0] != "files.storage.driver" {
		t.Fatalf("%+v", st)
	}
	// Invalid: last good stays, status reports the line.
	write("files:\n  window: soon\n")
	sc.Check(ctx)
	if st := sc.Status(); st.OK || st.ErrorLine != 2 || svc.Get().Storage.Driver != "s3" {
		t.Fatalf("%+v", st)
	}
	// Removed: back to env.
	_ = os.Remove(path)
	sc.Check(ctx)
	if svc.Get().Account.QuotaBytes != 5e9 || svc.Source() != "env" {
		t.Fatal("env not restored")
	}
}
