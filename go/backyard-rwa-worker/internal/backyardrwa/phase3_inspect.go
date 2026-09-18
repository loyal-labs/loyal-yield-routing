package backyardrwa

import "encoding/json"

// InspectPhase3Runtime resolves requested lanes through the production route
// resolver. It is a local capability measurement, not execution, authorization
// or deployment proof. It opens no RPC/database connection and loads no signer.
func InspectPhase3Runtime(lanes []string) ([]byte, error) {
	type laneResult struct {
		Lane     string        `json:"lane"`
		Resolved bool          `json:"resolved"`
		Binding  *RuntimeRoute `json:"binding,omitempty"`
		Reason   string        `json:"reason,omitempty"`
	}
	rows := make([]laneResult, 0, len(lanes))
	for _, lane := range lanes {
		row := laneResult{Lane: lane}
		route, err := runtimeRoute(lane)
		if err != nil {
			row.Reason = "RUNTIME_BINDING_MISSING"
		} else {
			row.Resolved = true
			row.Binding = &route
		}
		rows = append(rows, row)
	}
	return json.Marshal(struct {
		Schema     string       `json:"schema"`
		ReadOnly   bool         `json:"readOnly"`
		ProofLevel string       `json:"proofLevel"`
		GoalID     string       `json:"goalId"`
		CapsMicros [3]int64     `json:"capsMicros"`
		Lanes      []laneResult `json:"lanes"`
	}{"loyal-backyard-rwa-phase3-runtime-inspection/v1", true, "LOCAL_ROUTE_RESOLUTION",
		Phase3GoalID, [3]int64{Phase3TransactionCapMicros, Phase3FamilyCapMicros, Phase3GoalCapMicros}, rows})
}
