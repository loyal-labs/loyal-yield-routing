package backyardrwa

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedManifestIsExactCheckedInManifest(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "docs", "manifests", "backyard-rwa-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, embeddedBackyardManifest) {
		t.Fatal("embedded runtime manifest drifted from docs/manifests/backyard-rwa-v1.json")
	}
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.executionBlocker() != ErrBridgePrerequisitesUnavailable {
		t.Fatal("incomplete manifest was executable")
	}
}
