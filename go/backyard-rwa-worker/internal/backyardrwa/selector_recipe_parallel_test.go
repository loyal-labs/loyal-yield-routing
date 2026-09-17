package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectorRecipeReadsBoundsConcurrencyAndChecksEveryError(t *testing.T) {
	for fail := 0; fail < 17; fail++ {
		started, release := make(chan struct{}, 17), make(chan struct{})
		var active, peak atomic.Int32
		calls := make([]atomic.Int32, 17)
		want := errors.New("failed input")
		done := make(chan error, 1)
		go func() {
			done <- selectorRecipeReads(17, func(i int) error {
				calls[i].Add(1)
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				started <- struct{}{}
				<-release
				active.Add(-1)
				if i == fail {
					return want
				}
				return nil
			})
		}()
		for i := 0; i < 4; i++ {
			select {
			case <-started:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("four reads did not start concurrently")
			}
		}
		if peak.Load() != 4 {
			close(release)
			t.Fatal("unexpected concurrency", peak.Load())
		}
		close(release)
		if err := <-done; !errors.Is(err, want) {
			t.Fatal("input error lost", fail, err)
		}
		if peak.Load() > 4 || active.Load() != 0 {
			t.Fatal("read bound or join failed")
		}
		for i := range calls {
			if calls[i].Load() != 1 {
				t.Fatal("missing or duplicate read", i)
			}
		}
	}
}

func TestSelectorRecipeParallelFeesStayBoundToMessagesAndSlots(t *testing.T) {
	_, _, allocate := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 1_000_000, 1_000_000, 0, 0)
	_, _, report := bridgeAdmissionFixture(t, ReportNAV, 0, 0, 0, 1_000_000)
	inputs := []*phase3BuildInput{
		selectorRecipeInput(t, allocate.Request, allocate.ExpectedEffects),
		selectorRecipeInput(t, report.Request, report.ExpectedEffects),
		selectorRecipeInput(t, allocate.Request, allocate.ExpectedEffects),
		selectorRecipeInput(t, report.Request, report.ExpectedEffects),
		selectorRecipeInput(t, report.Request, report.ExpectedEffects),
	}
	fees := map[string]uint64{}
	for _, input := range inputs {
		_, _, message, err := input.decode()
		if err != nil {
			t.Fatal(err)
		}
		k := sha256Bytes(message)
		if fees[k] == 0 {
			fees[k] = uint64(5000 + len(fees)*1234)
		}
	}
	if len(fees) != 2 {
		t.Fatal("fixture needs distinct compiled messages")
	}
	for _, tc := range []struct {
		name                          string
		feeSlot, priceSlot, finalSlot int64
		invalid                       bool
	}{
		{"exact messages", 42, 42, 43, false},
		{"future fee", 44, 42, 43, true},
		{"stale fee", 41, 42, 43, true},
		{"future price", 42, 44, 43, true},
		{"stale price", 42, 41, 43, true},
		{"expired sample", 42, 42, 75, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := budgetBuildRPC(t, 5000, 42)
			base := rpc.client.Transport
			var feeReads atomic.Int32
			rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				r.Body = io.NopCloser(bytes.NewReader(raw))
				var body struct {
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				if err = json.Unmarshal(raw, &body); err != nil {
					return nil, err
				}
				var result any
				switch body.Method {
				case "getSlot":
					result = tc.finalSlot
				case "getFeeForMessage":
					feeReads.Add(1)
					var encoded string
					if err = json.Unmarshal(body.Params[0], &encoded); err != nil {
						return nil, err
					}
					message, err := base64.StdEncoding.DecodeString(encoded)
					if err != nil {
						return nil, err
					}
					fee, ok := fees[sha256Bytes(message)]
					if !ok {
						return nil, errors.New("unknown exact message")
					}
					result = map[string]any{"context": map[string]int64{"slot": tc.feeSlot}, "value": fee}
				case "getMultipleAccounts":
					response, err := base.RoundTrip(r)
					if err != nil {
						return nil, err
					}
					defer response.Body.Close()
					var envelope struct {
						Result struct {
							Context map[string]int64 `json:"context"`
							Value   any              `json:"value"`
						} `json:"result"`
					}
					if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
						return nil, err
					}
					envelope.Result.Context["slot"] = tc.priceSlot
					result = envelope.Result
				default:
					return nil, errors.New("unexpected RPC")
				}
				payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
				if err != nil {
					return nil, err
				}
				return response(string(payload)), nil
			})
			got, err := priceSelectorRecipe(context.Background(), rpc, SelectedRouteID, inputs, 42)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid observation admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if feeReads.Load() != int32(len(inputs)) || len(got.Costs) != len(inputs) {
				t.Fatal("repeated exact messages omitted")
			}
			var total uint64
			for i, cost := range got.Costs {
				_, _, message, err := inputs[i].decode()
				if err != nil {
					t.Fatal(err)
				}
				expected := fees[sha256Bytes(message)]
				total += expected
				if cost.Fee.Lamports != expected || cost.Fee.MessageSHA256 != sha256Bytes(message) || cost.Fee.Slot != 42 || cost.NativePrice.ObservedSlot != 42 || cost.ValidThroughSlot != 74 {
					t.Fatal("fee mapping or original source freshness lost", i, cost)
				}
			}
			if got.NetworkLamports != total || got.ValidThroughSlot != 74 {
				t.Fatal("fee sum or oldest expiry changed")
			}
			// A bad late recipe step must fail before any RPC is started.
			feeReads.Store(0)
			_, err = priceSelectorRecipe(context.Background(), rpc, SelectedRouteID, append(inputs, nil), 42)
			if err == nil || feeReads.Load() != 0 {
				t.Fatal("read started before full recipe validation")
			}
		})
	}
}
