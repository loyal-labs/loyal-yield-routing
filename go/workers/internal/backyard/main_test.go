package backyard

import (
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	landResendEvery = time.Millisecond
	os.Exit(m.Run())
}
