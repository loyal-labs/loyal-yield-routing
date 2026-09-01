package backyardrwa

import "fmt"

// RuntimeBlocker identifies an external prerequisite that is intentionally not
// guessed by the worker. It is distinct from a malformed snapshot: callers may
// retry after the stated prerequisite has been independently satisfied.
type RuntimeBlocker struct {
	Code            string
	ResumeCondition string
}

func (b *RuntimeBlocker) Error() string {
	if b == nil {
		return ""
	}
	return fmt.Sprintf("backyard RWA runtime blocked: %s", b.Code)
}

var (
	// Never turn a discovery address into an executable route. Phase 1 needs
	// only the authenticated bridge plus the exact PRIME/USDC entry and exit
	// policies; the broader catalog is a separate, nonblocking release.
	ErrBridgePrerequisitesUnavailable = &RuntimeBlocker{
		Code:            "BLOCKED_BRIDGE_PREREQUISITES",
		ResumeCondition: "confirm the immutable adaptor v2 config and exact Phase 1 bridge and PRIME/USDC policy bindings on mainnet",
	}
)
