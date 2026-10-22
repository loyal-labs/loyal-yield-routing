package earnmax

import "time"

type WithdrawalHealth struct {
	Version      int        `json:"version"`
	Cluster      string     `json:"cluster"`
	Vault        string     `json:"vault"`
	Program      string     `json:"program"`
	RouteKey     string     `json:"routeKey"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	ObservedAt   time.Time  `json:"observedAt"`
	ObservedSlot int64      `json:"observedSlot"`
	BlockedSince *time.Time `json:"blockedSince"`
}
