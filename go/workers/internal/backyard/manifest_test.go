package backyard

import "testing"

// The manifest is the registry's: its identity is the hash of every lane's
// chain facts, so a changed fact is a new identity.
func TestManifestIdentityIsTheRegistrysFacts(t *testing.T) {
	t.Parallel()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil || !sha256Pattern.MatchString(manifest.SHA256) || manifest.RuntimeActivation.SelectedLane != SelectedRouteID {
		t.Fatalf("%+v %v", manifest, err)
	}
	changed := *rwa
	changed.routes = append([]RuntimeRoute(nil), rwa.routes...)
	changed.routes[0].Kamino.Obligation = bridgeVault
	if changed, _ := manifestOf(&changed); changed.SHA256 == manifest.SHA256 {
		t.Fatal("a changed lane fact kept the manifest identity")
	}
}
