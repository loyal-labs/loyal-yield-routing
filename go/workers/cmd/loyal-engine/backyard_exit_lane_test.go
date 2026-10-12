package main

import "testing"

// The exit row is written only with an operator name, or cleared, never both,
// and a cleared row cannot also ask for equity 0.
func TestExitLaneFlagsNameTheOperatorOrClear(t *testing.T) {
	request, err := parseExitLaneFlags([]string{"--lane", "AUTO/AUTO/wYLDS", "--by", "operator", "--equity-zero"})
	if err != nil || request.Lane != "AUTO/AUTO/wYLDS" || request.RequestedBy != "operator" || !request.EquityZero || request.Clear {
		t.Fatalf("exit row = %+v, %v", request, err)
	}
	if request, err := parseExitLaneFlags([]string{"--lane", "AUTO/AUTO/wYLDS", "--clear"}); err != nil || !request.Clear {
		t.Fatalf("clear = %+v, %v", request, err)
	}
	for _, args := range [][]string{
		{"--lane", "AUTO/AUTO/wYLDS"},
		{"--lane", "AUTO/AUTO/wYLDS", "--by", "operator", "--clear"},
		{"--lane", "AUTO/AUTO/wYLDS", "--clear", "--equity-zero"},
		{"--by", "operator"},
		{"--lane", "AUTO/AUTO/wYLDS", "--by"},
		{"--lane", "AUTO/AUTO/wYLDS", "--by", "operator", "--execute"},
	} {
		if _, err := parseExitLaneFlags(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
