package integration

import (
	"os"
	"testing"

	"github.com/boson-chat/files/internal/logger"
)

func TestMain(m *testing.M) {
	level := os.Getenv("TEST_LOG_LEVEL")
	if level == "" {
		level = "error"
	}
	logger.Logger(logger.WithServerName("files"), logger.WithEnvironment("test"), logger.WithLevel(level))
	os.Exit(m.Run())
}
