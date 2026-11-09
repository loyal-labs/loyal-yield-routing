package admin

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEarnMaxWorkerVersion(t *testing.T) {
	for _, tc := range []struct{ owner, want string }{
		{"worker:backyard:rwa:sha-579af446b0fdcb783e9befb366660342647ed5b4", "579af44"},
		{"worker:backyard:rwa", ""},
		{"worker:backyard:rwa:sha-abcd", ""},
		{"worker:backyard:rwa:sha-" + strings.Repeat("z", 40), ""},
	} {
		got := earnMaxWorkerVersion(&tc.owner)
		if (got == nil && tc.want != "") || (got != nil && *got != tc.want) {
			t.Fatalf("owner %q: version %v, want %q", tc.owner, got, tc.want)
		}
	}
	if earnMaxWorkerVersion(nil) != nil {
		t.Fatal("unrecorded release reported as known")
	}
}

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
			got, ok := earnMaxDecodeReport(tc.program, tc.returnData, tc.request, program)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("NAV=(%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestEarnMaxVaultSelection(t *testing.T) {
	loyal, err := earnMaxConfigFor("")
	if err != nil {
		t.Fatal(err)
	}
	backyard, err := earnMaxConfigFor("backyard")
	if err != nil {
		t.Fatal(err)
	}
	if loyal.identity.Address != "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA" || backyard.identity.Address != "GrHZD15bMiUwrVPsmBwz8GRoQeDZN6wNEQVdW8k8SxV1" {
		t.Fatal("vault selection changed")
	}
	if loyal.RouteKey == backyard.RouteKey || loyal.identity.CashATA == backyard.identity.CashATA || loyal.Accounts.LPMint == backyard.Accounts.LPMint || earnMaxCaches[loyal.identity.ID] == earnMaxCaches[backyard.identity.ID] {
		t.Fatal("vaults share data or a cache")
	}
	if _, err := earnMaxConfigFor("unknown"); err == nil {
		t.Fatal("unknown vault silently selected a different vault")
	}
	data := "Fc1bBwAAAAA="
	for _, cfg := range []earnMaxConfig{loyal, backyard} {
		program := cfg.Accounts.AdaptorProgram.String()
		if _, ok := earnMaxDecodeReport(&program, &data, nil, program); !ok {
			t.Fatal("selected adaptor report rejected")
		}
		if _, ok := earnMaxDecodeReport(&program, &data, nil, "another-adaptor"); ok {
			t.Fatal("another adaptor's report was accepted")
		}
	}
}
