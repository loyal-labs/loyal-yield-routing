package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var releasePattern = regexp.MustCompile(`^sha-[0-9a-f]{40}$`)
var instancePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

// Release is the immutable build identity, stamped by the linker:
//
//	-ldflags "-X github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine.Release=sha-<commit>"
var Release string

// InstanceOwner identifies a process instance and immutable release. Authority
// still comes from the family's database lease generation, not this string.
func InstanceOwner(scope, instance, release string) (string, error) {
	if scope != "retail" && scope != "backyard" && scope != "observer" {
		return "", fmt.Errorf("unsupported worker scope")
	}
	if !instancePattern.MatchString(instance) || !releasePattern.MatchString(release) {
		return "", fmt.Errorf("invalid instance or immutable release")
	}
	return "worker:" + scope + ":" + instance + ":" + release, nil
}

// Credential reads one secret from the directory systemd fills from
// LoadCredentialEncrypted=. Secrets never travel through the environment.
func Credential(name string) (string, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", fmt.Errorf("credential %s missing: CREDENTIALS_DIRECTORY is not set", name)
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("credential %s missing", name)
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("credential %s is empty", name)
	}
	return value, nil
}
