package config

import (
	"testing"
	"time"
)

// LoadConfig with nothing set must give the tag defaults and pass Validate:
// the image starts that way (the stack only sets what it changes).
func TestLoadConfigDefaults(t *testing.T) {
	for _, k := range []string{"CORS_ALLOWED_ORIGINS", "ORIGINS_URL", "FILES_EXPIRY_EVERY", "FILES_PENDING_TIMEOUT", "ORIGINS_REFRESH"} {
		t.Setenv(k, "")
	}
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.WorkerConfig.ExpiryEvery != time.Minute || c.WorkerConfig.PendingTimeout != time.Hour {
		t.Errorf("worker defaults: every=%v pending=%v", c.WorkerConfig.ExpiryEvery, c.WorkerConfig.PendingTimeout)
	}
	if c.HTTPConfig.CORSAllowedOrigins != "*" || c.HTTPConfig.OriginsRefresh != 5*time.Minute {
		t.Errorf("http defaults: cors=%q refresh=%v", c.HTTPConfig.CORSAllowedOrigins, c.HTTPConfig.OriginsRefresh)
	}
	if c.HTTPConfig.UploadTimeout != 30*time.Minute {
		t.Errorf("upload timeout %v", c.HTTPConfig.UploadTimeout)
	}
}
