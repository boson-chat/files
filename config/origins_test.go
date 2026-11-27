package config

import (
	"strings"
	"testing"
)

func TestCORSDirectoryNeedsOriginsURL(t *testing.T) {
	if (HTTPConfig{CORSAllowedOrigins: "*"}).UsesDirectory() {
		t.Fatal("* uses the directory")
	}
	c := Config{HTTPConfig: HTTPConfig{CORSAllowedOrigins: "https://chat.a.test, Directory"}}
	if !c.HTTPConfig.UsesDirectory() {
		t.Fatal("UsesDirectory")
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "ORIGINS_URL") {
		t.Fatalf("no ORIGINS_URL: %v", err)
	}
	c.HTTPConfig.OriginsURL = "http://auth:8080/v1/web-origins"
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "ORIGINS_URL") {
		t.Fatal(err)
	}
}
