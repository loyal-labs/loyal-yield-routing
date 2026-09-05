package backyardrwa

import (
	"context"
	"encoding/json"
	"fmt"
)

// InspectPhase3SetupRent measures existing equivalent policy allocations.
// It does not claim that a new allocation must use this size: the compiler
// and real-program creation probe must establish that separately. No signer,
// database, transaction creation or send path is involved.
func InspectPhase3SetupRent(ctx context.Context, endpoint string) ([]byte, error) {
	rpc, err := NewRPCClient(endpoint)
	if err != nil {
		return nil, fmt.Errorf("setup rent inspection: invalid RPC configuration")
	}
	var genesis string
	if err = rpc.call(ctx, "getGenesisHash", []any{}, &genesis); err != nil {
		return nil, fmt.Errorf("setup rent inspection: genesis read failed")
	}
	if genesis != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return nil, fmt.Errorf("setup rent inspection requires mainnet-beta")
	}
	minimumSlot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup rent inspection: initial slot read failed")
	}
	addresses := []string{"2m7DpWN1d7UC8iMZyipGzo5SRaBz9Buqhw1VJUTMpLSV", "AjjV5p7BPCxqaf92EsUjx2bavkTuhjHwiBJMvk8Gh8Uo"}
	slot, accounts, err := rpc.GetMultipleAccounts(ctx, addresses, minimumSlot)
	if err != nil {
		return nil, fmt.Errorf("setup rent inspection: policy account read failed")
	}
	price, err := ObserveNativeSOLBudgetPrice(ctx, rpc, slot)
	if err != nil {
		return nil, fmt.Errorf("setup rent inspection: native price observation failed")
	}
	type row struct {
		Address                     string `json:"address"`
		DataLength                  int    `json:"dataLength"`
		DataSHA256                  string `json:"dataSha256"`
		RentLamports                uint64 `json:"rentLamports"`
		RentUpperMicros             int64  `json:"rentUpperMicros"`
		RentAloneFitsTransactionCap bool   `json:"rentAloneFitsTransactionCap"`
	}
	rows := make([]row, 0, len(accounts))
	for _, account := range accounts {
		if account.Owner != bridgeSquadsProgram || account.Executable || len(account.Data) == 0 {
			return nil, fmt.Errorf("setup policy account envelope mismatch")
		}
		var rent uint64
		if err = rpc.call(ctx, "getMinimumBalanceForRentExemption", []any{len(account.Data), map[string]string{"commitment": "confirmed"}}, &rent); err != nil {
			return nil, fmt.Errorf("setup rent inspection: rent read failed")
		}
		value, err := price.valueUpper(rent, nativeSOLBudgetAsset, "11111111111111111111111111111111", price.ObservedSlot)
		if err != nil {
			return nil, fmt.Errorf("setup rent inspection: rent valuation failed")
		}
		rows = append(rows, row{account.Address, len(account.Data), sha256Bytes(account.Data), rent, value, value <= Phase3TransactionCapMicros})
	}
	endSlot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return nil, fmt.Errorf("setup rent inspection: final slot read failed")
	}
	if endSlot < price.ObservedSlot || endSlot > price.ValidThroughSlot {
		return nil, budgetHold("setup_rent_valuation_expired")
	}
	return json.Marshal(struct {
		Schema     string      `json:"schema"`
		ReadOnly   bool        `json:"readOnly"`
		Slot       int64       `json:"slot"`
		Price      BudgetPrice `json:"price"`
		Policies   []row       `json:"policies"`
		Limitation string      `json:"limitation"`
	}{"loyal-backyard-rwa-phase3-setup-rent-inspection/v1", true, endSlot, price, rows,
		"Existing allocation samples only; prove required new allocation size with the real-program PolicyCreate path. Network fees are additional."})
}
