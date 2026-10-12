package backyard

import (
	"testing"
)

func TestEmbeddedManifestIsExecutable(t *testing.T) {
	t.Parallel()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if blocker := manifest.executionBlocker(); blocker != nil {
		t.Fatalf("v2 manifest is blocked after the policy install readback: %v", blocker)
	}
}
