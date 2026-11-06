package chain

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

func TestClaimRPCUnknownAndLagAreNotExpiryOrFreeFees(t *testing.T) {
	for _, method := range []string{"getFeeForMessage", "isBlockhashValid"} {
		for _, tc := range []struct {
			name    string
			slot    uint64
			value   any
			wantErr bool
			behind  bool
		}{
			{"missing", 90, nil, true, false},
			{"lagging", 89, false, true, true},
			{"known", 90, false, false, false},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				message := []byte{7, 8, 9}
				client := serve(t, func(req request) (int, any) {
					if req.Method != method {
						t.Fatalf("unexpected method %s", req.Method)
					}
					var opts struct {
						Commitment string `json:"commitment"`
						MinSlot    uint64 `json:"minContextSlot"`
					}
					if len(req.Params) != 2 || json.Unmarshal(req.Params[1], &opts) != nil || opts.Commitment != "finalized" || opts.MinSlot != 90 {
						t.Fatal("claim safety read lost commitment/context boundary")
					}
					value := tc.value
					if method == "getFeeForMessage" {
						var encoded string
						if json.Unmarshal(req.Params[0], &encoded) != nil || encoded != base64.StdEncoding.EncodeToString(message) {
							t.Fatal("fee quote changed signed message")
						}
						if value != nil {
							value = uint64(0)
						}
					}
					return http.StatusOK, result(map[string]any{"context": map[string]any{"slot": tc.slot}, "value": value})
				})
				var err error
				if method == "getFeeForMessage" {
					_, _, err = client.QuotedMessageFee(t.Context(), message, rpc.CommitmentFinalized, 90)
				} else {
					_, _, err = client.BlockhashValid(t.Context(), solana.Hash{1}, rpc.CommitmentFinalized, 90)
				}
				if (err != nil) != tc.wantErr || tc.behind && !errors.Is(err, ErrBehind) {
					t.Fatalf("unexpected safety result: %v", err)
				}
			})
		}
	}
}
