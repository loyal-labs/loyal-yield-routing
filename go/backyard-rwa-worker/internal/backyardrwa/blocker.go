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
	// The checked-in manifest declares the v2 strategy config and policy graph
	// unresolved. Do not turn a discovery address into an executable route.
	ErrBridgePrerequisitesUnavailable = &RuntimeBlocker{
		Code:            "BLOCKED_BRIDGE_PREREQUISITES",
		ResumeCondition: "confirm the immutable adaptor v2 config, complete policy catalog, and accepted report sequence from mainnet",
	}
	// There is no local, independently checked Kamino Multiply packet builder or
	// current reserve fixture. Constructing an approximation would turn a risk
	// decision into a money-moving guess.
	ErrKaminoTransactionConstructionUnavailable = &RuntimeBlocker{
		Code:            "BLOCKED_KAMINO_TRANSACTION_CONSTRUCTION",
		ResumeCondition: "provide a confirmed PRIME/USDC Kamino account graph and exact signed-simulation fixture for every Multiply instruction",
	}
)
