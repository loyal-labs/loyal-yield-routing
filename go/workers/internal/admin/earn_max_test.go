package admin

import (
	"encoding/base64"
	"testing"
)

func TestEarnMaxReportNAVFormats(t *testing.T) {
	program := "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"
	current := "Fc1bBwAAAAA=" // 123456789 USDC micros, as returned by the adaptor.
	zero := "AAAAAAAAAAA="
	legacy := base64.StdEncoding.EncodeToString([]byte(`{"Report":{"NAVAfterRaw":987654321}}`))
	wrongProgram, malformed, short, long := "another-program", "!", "AQ==", "AAAAAAAAAAAA"
	missing := base64.StdEncoding.EncodeToString([]byte(`{"Report":{}}`))
	for _, tc := range []struct {
		name                         string
		program, returnData, request *string
		want                         float64
		ok                           bool
	}{
		{"current", &program, &current, nil, 123456789, true},
		{"current takes precedence", &program, &current, &legacy, 123456789, true},
		{"zero is a reported value", &program, &zero, nil, 0, true},
		{"legacy", nil, nil, &legacy, 987654321, true},
		{"wrong program", &wrongProgram, &current, &legacy, 0, false},
		{"missing program", nil, &current, nil, 0, false},
		{"malformed return", &program, &malformed, &legacy, 0, false},
		{"truncated return", &program, &short, nil, 0, false},
		{"oversized return", &program, &long, nil, 0, false},
		{"missing NAV is not zero", nil, nil, &missing, 0, false},
		{"missing report", nil, nil, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := earnMaxDecodeReport(tc.program, tc.returnData, tc.request)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NAV=(%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}
