package fleet

import "strconv"

// CI maps its task-owned PostgreSQL service to a dynamic local port. The
// callers still require their exact database, loopback host and test role.
func registeredFleetFixturePort(port string) bool {
	value, err := strconv.Atoi(port)
	return err == nil && value >= 1024 && value <= 65535
}
