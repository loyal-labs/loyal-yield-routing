package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var releasePattern = regexp.MustCompile(`^sha-[0-9a-f]{40}$`)
var instancePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

var Release string

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

// OptionalCredential is Credential for a secret that may be absent: no
// credentials directory, or no file of that name, reads as "".
func OptionalCredential(name string) (string, error) {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", fmt.Errorf("cannot inspect credential %s", name)
	}
	return Credential(name)
}
