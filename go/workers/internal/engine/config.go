package engine

import (
	"fmt"
	"regexp"
)

var releasePattern = regexp.MustCompile(`^sha-[0-9a-f]{40}$`)
var instancePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,80}$`)

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
