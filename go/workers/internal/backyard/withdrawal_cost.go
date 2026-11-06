package backyard

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

type withdrawalCostParticipant struct {
	Consent         earnmax.WithdrawalConsent `json:"consent"`
	ID              string                    `json:"id"`
	EscrowLP        uint64                    `json:"escrowLp"`
	Payable         earnmax.USDCRaw           `json:"payable"`
	MaxCost         earnmax.USDCRaw           `json:"maxCost"`
	IncrementalCost earnmax.USDCRaw           `json:"incrementalCost"`
}

type withdrawalCostPreparation struct {
	Participants            []withdrawalCostParticipant `json:"participants"`
	EffectiveLP             uint64                      `json:"effectiveLp"`
	MaxCost                 earnmax.USDCRaw             `json:"maxCost"`
	OtherHolders            earnmax.USDCRaw             `json:"otherHolders"`
	IncrementalCost         earnmax.USDCRaw             `json:"incrementalCost"`
	IncrementalOtherHolders earnmax.USDCRaw             `json:"incrementalOtherHolders"`
	NetworkUSDC             *earnmax.USDCRaw            `json:"networkUsdc"`
	NetworkUnknownReason    string                      `json:"networkUnknownReason,omitempty"`
	NetworkLamports         uint64                      `json:"networkLamports"`
	ReceiptFingerprint      string                      `json:"receiptFingerprint"`
	Slot                    int64                       `json:"slot"`
	MessageSHA256           string                      `json:"messageSha256"`
	net                     *certifiedNAV
	view                    *View
	manifest                RouteManifest
	chain                   *chain.Client
}

type withdrawalCostRequest struct {
	request any
	cost    *withdrawalCostPreparation
}

func withdrawalUnderlyingRequest(request any) any {
	if r, ok := request.(withdrawalCostRequest); ok {
		return r.request
	}
	return request
}

func validateWithdrawalRequestNAV(manifest RouteManifest, request any, now time.Time, slot int64) error {
	if a, required := requestAddAdmission(request); required {
		if err := validateAddAdmission(manifest, request, a, now, slot); err != nil {
			return err
		}
	}
	if r, ok := request.(BridgeBuildRequest); ok && (r.Action != bridgeStageSquadsToVoltr || manifest.vault.trustful()) {
		if !r.net.valid(now, slot) {
			return staleHold("certified_nav_expired")
		}
		return r.validateNAV(manifest.vault, now)
	}
	return nil
}

func (p *withdrawalCostPreparation) validateSigningCertificate(request any, message []byte, now time.Time, slot int64) error {
	if p == nil || sha256Bytes(message) != p.MessageSHA256 {
		return staleHold("withdrawal_cost_evidence_mismatch")
	}
	if !p.net.valid(now, slot) {
		return staleHold("withdrawal_cost_evidence_expired")
	}
	for _, r := range p.Participants {
		reason, err := r.Consent.Admit(earnmax.WithdrawalAdmission{ProjectedPostFeePayable: &r.Payable, NewIncrementalCost: r.IncrementalCost}, now)
		if err != nil {
			return err
		}
		if reason != "" {
			return withdrawalCostWait(reason)
		}
	}
	return validateWithdrawalRequestNAV(p.manifest, request, now, slot)
}

func ordinaryWithdrawalCostRequired(s Snapshot, d Decision) bool {
	if d.Reason == hardLTVDeleverReason || s.Target.Rule == ruleHardLTV || s.Target.Rule == ruleTermMaturity || s.Target.Rule == ruleOperatorExit {
		return false
	}
	if s.Target.Rule != ruleWithdrawal && !(s.Target.Rule == ruleCustody && withdrawalShortfallRaw(s) > 0) {
		return false
	}
	switch d.Action {
	case Hold, HoldManualRecovery, ReportNAV, RecoverTransaction:
		return false
	}
	return true
}

func prepareWithdrawalCost(ctx context.Context, database *Database, rpc *chain.Client, view *View, manifest RouteManifest, d Decision, o Observation, request any, effects ExpectedEffects) (any, error) {
	if !ordinaryWithdrawalCostRequired(o.Snapshot, d) {
		return request, nil
	}
	if database == nil || rpc == nil || view == nil || o.routeBatch == nil || !o.Snapshot.netKnown(time.Now()) {
		return nil, staleHold("withdrawal_cost_unavailable")
	}
	message, _, _, err := compileRequest(manifest, request)
	if err != nil {
		return nil, err
	}
	fee, err := chain.MessageFee(message)
	if err != nil {
		return nil, staleHoldFrom(err, "withdrawal_fee_unavailable")
	}
	var network *earnmax.USDCRaw
	networkUnknown := "operator_fee_price_unavailable"
	if price, priceErr := ObserveNativeSOLBudgetPrice(ctx, rpc, view, o.Snapshot.Slot); priceErr == nil {
		if value, valueErr := (lanePricer{sol: price}).lamportsMicros(fee); valueErr == nil && value >= 0 {
			raw := earnmax.USDCRaw(value)
			network = &raw
			networkUnknown = ""
		}
	}
	total, incremental, err := withdrawalOperationCost(request, effects, o.Snapshot)
	if err != nil {
		return nil, err
	}
	p := &withdrawalCostPreparation{Slot: o.Snapshot.Slot, MaxCost: total, NetworkUSDC: network, NetworkUnknownReason: networkUnknown, NetworkLamports: fee, MessageSHA256: sha256Bytes(message), net: o.Snapshot.net, view: view, manifest: manifest, chain: rpc}
	if err := p.load(ctx, database, rpc, o); err != nil {
		return nil, err
	}
	if err := p.allocate(total, incremental); err != nil {
		return nil, err
	}
	return withdrawalCostRequest{request, p}, nil
}

func withdrawalOperationCost(request any, effects ExpectedEffects, s Snapshot) (earnmax.USDCRaw, earnmax.USDCRaw, error) {
	if effects.Sale == nil {
		switch request.(type) {
		case BridgeBuildRequest, KaminoLegRequest:
			return 0, 0, nil
		}
		return 0, 0, staleHold("withdrawal_sale_cost_unavailable")
	}
	var minimum uint64
	switch r := request.(type) {
	case StepRequest:
		if r.Swap == nil && r.Hastra == nil {
			return 0, 0, staleHold("withdrawal_sale_cost_unavailable")
		}
		var err error
		minimum, err = stepConversionOutput(r.conversion())
		if err != nil {
			return 0, 0, err
		}
	case JupiterSwapRequest:
		minimum = r.MinimumOutputRaw
	case HastraConversionRequest:
		var err error
		minimum, err = stepConversionOutput(r)
		if err != nil {
			return 0, 0, err
		}
	default:
		return 0, 0, staleHold("withdrawal_sale_cost_unavailable")
	}
	proceeds, err := effects.Sale.proceedsValue(minimum)
	if err != nil {
		return 0, 0, staleHoldFrom(err, "withdrawal_sale_cost_unavailable")
	}
	loss := uint64(0)
	if proceeds < effects.Sale.MarkValueRaw {
		loss = effects.Sale.MarkValueRaw - proceeds
	}
	// Only the matching scoped full-holding quote contributes an allowance.
	allowance := uint64(0)
	if s.net != nil {
		for _, q := range s.net.quotes {
			if q.Lane == effects.Sale.Lane && q.MarkSF == effects.Sale.MarkSF && q.QuotedCollateralRaw >= effects.Sale.SoldRaw {
				n := new(big.Int).Mul(new(big.Int).SetUint64(effects.Sale.MarkValueRaw), new(big.Int).SetUint64(uint64(q.BPS)))
				n.Quo(n, new(big.Int).SetUint64(uint64(microBPSWhole)))
				if !n.IsUint64() {
					return 0, 0, staleHold("withdrawal_cost_overflow")
				}
				allowance = n.Uint64()
			}
		}
	}
	if loss > uint64(math.MaxInt64) {
		return 0, 0, staleHold("withdrawal_cost_overflow")
	}
	return earnmax.USDCRaw(loss), earnmax.USDCRaw(loss - min(loss, allowance)), nil
}

func (p *withdrawalCostPreparation) load(ctx context.Context, database *Database, rpc *chain.Client, o Observation) error {
	v := p.manifest.vault
	if v.VoltrVault != earnmax.Vault.String() {
		return staleHold("withdrawal_generation_verifier_unavailable")
	}
	slot, _, receipts, err := p.view.readWithReceipts(ctx, v, nil, p.Slot)
	if err != nil {
		return err
	}
	demand, fingerprint, err := decodeConfirmedWithdrawalDemand(v, receipts)
	if err != nil || demand != o.Snapshot.WithdrawalDemandRaw || !p.net.valid(time.Now(), slot) {
		return staleHold("withdrawal_generation_changed")
	}
	p.ReceiptFingerprint = fingerprint
	vaultAccount := accountAt(o.routeBatch.Accounts, v.VoltrVault)
	vault, err := voltr.DecodeVault(chainAccount(vaultAccount, v.VoltrVault))
	if err != nil {
		return staleHoldFrom(err, "withdrawal_cost_unavailable")
	}
	mint := accountAt(o.routeBatch.Accounts, v.lpMint)
	if len(mint.Data) < 82 || mint.Owner != classicTokenProgram {
		return staleHold("withdrawal_lp_supply_unavailable")
	}
	supply := binary.LittleEndian.Uint64(mint.Data[36:44])
	now := time.Unix(o.Snapshot.ChainUnix, 0)
	if o.Snapshot.ChainUnix <= 0 || o.Snapshot.TotalVaultNAVRaw < 0 {
		return staleHold("withdrawal_cost_unavailable")
	}
	effective, err := vault.TotalLP(supply, now)
	if err != nil || !effective.IsUint64() || effective.Sign() <= 0 {
		return staleHold("withdrawal_lp_supply_unavailable")
	}
	p.EffectiveLP = effective.Uint64()
	// Current certified pool NAV already contains all exit allowances. The
	// receipt ceiling and Voltr redemption/locked-profit terms still apply.
	vault.TotalValueRaw = uint64(o.Snapshot.TotalVaultNAVRaw)
	reader := earnmax.NewReader(rpc, database.pool)
	for _, raw := range receipts {
		receipt, err := DecodeVoltrWithdrawalReceipt(raw.Account, raw.Address, v.VoltrVault, bridgeCapRaw)
		if err != nil {
			return err
		}
		if receipt.LPEscrowedRaw == 0 {
			continue
		}
		current, err := earnmax.ReadWithdrawalRequest(ctx, rpc, receipt.User, uint64(p.Slot))
		if err != nil {
			return staleHoldFrom(err, "withdrawal_generation_unavailable")
		}
		var body []byte
		err = database.pool.QueryRow(ctx, `SELECT payload FROM loyal_yield.earn_max_withdrawal_consents WHERE authority=$1 AND vault=$2 AND request_signature=$3 AND request_instruction=$4 ORDER BY version DESC LIMIT 1`, receipt.User.String(), v.VoltrVault, current.RequestSignature, current.RequestInstruction).Scan(&body)
		if err != nil {
			return staleHoldFrom(err, "withdrawal_needs_consent")
		}
		var consent earnmax.WithdrawalConsent
		if json.Unmarshal(body, &consent) != nil {
			return staleHold("withdrawal_needs_consent")
		}
		owner, e1 := solana.PublicKeyFromBase58(consent.Owner)
		settings, e2 := solana.PublicKeyFromBase58(consent.Settings)
		if e1 != nil || e2 != nil || consent.Authority != receipt.User.String() || consent.LPRaw != strconv.FormatUint(receipt.LPEscrowedRaw, 10) || consent.Validate(owner, settings, time.Now()) != nil {
			return staleHold("withdrawal_needs_consent")
		}
		if err := reader.VerifyWithdrawalGeneration(ctx, consent); err != nil {
			return staleHoldFrom(err, "withdrawal_generation_unavailable")
		}
		payable, err := vault.LPToAssets(receipt.LPEscrowedRaw, supply, now)
		ceiling, ok := receipt.AssetToWithdraw.FloorRaw()
		if err != nil || !ok || payable > math.MaxInt64 {
			return staleHold("withdrawal_payable_unavailable")
		}
		id, err := consent.Digest()
		if err != nil {
			return err
		}
		p.Participants = append(p.Participants, withdrawalCostParticipant{Consent: consent, ID: id, EscrowLP: receipt.LPEscrowedRaw, Payable: earnmax.USDCRaw(min(payable, ceiling))})
	}
	if len(p.Participants) == 0 {
		return staleHold("withdrawal_generation_unavailable")
	}
	sort.Slice(p.Participants, func(i, j int) bool {
		return p.Participants[i].Consent.WithdrawalGeneration.Key(p.Participants[i].Consent.Authority) < p.Participants[j].Consent.WithdrawalGeneration.Key(p.Participants[j].Consent.Authority)
	})
	return nil
}

func (p *withdrawalCostPreparation) allocate(total, incremental earnmax.USDCRaw) error {
	participants := make([]earnmax.WithdrawalCostParticipant, len(p.Participants))
	for i, r := range p.Participants {
		participants[i] = earnmax.WithdrawalCostParticipant{ConsentID: r.ID, EscrowedLP: r.EscrowLP}
	}
	if incremental < 0 || incremental > total {
		return staleHold("withdrawal_cost_unavailable")
	}
	covered, err := earnmax.AllocateWithdrawalCost(total-incremental, p.EffectiveLP, participants)
	if err != nil {
		return err
	}
	inc, err := earnmax.AllocateWithdrawalCost(incremental, p.EffectiveLP, participants)
	if err != nil {
		return err
	}
	p.OtherHolders = covered.OtherHolders + inc.OtherHolders
	p.MaxCost = total
	p.IncrementalCost = incremental
	p.IncrementalOtherHolders = inc.OtherHolders
	for i := range p.Participants {
		for j := range covered.Requests {
			if covered.Requests[j].ConsentID == p.Participants[i].ID {
				p.Participants[i].MaxCost = covered.Requests[j].Cost + inc.Requests[j].Cost
				p.Participants[i].IncrementalCost = inc.Requests[j].Cost
			}
		}
	}
	return nil
}

func (p *withdrawalCostPreparation) validateAccounting() error {
	if p == nil || p.EffectiveLP == 0 || p.MaxCost < 0 || p.IncrementalCost < 0 || p.IncrementalCost > p.MaxCost || (p.NetworkUSDC != nil && *p.NetworkUSDC < 0) || p.NetworkLamports == 0 || p.OtherHolders < 0 || p.IncrementalOtherHolders < 0 || !sha256Pattern.MatchString(p.MessageSHA256) || len(p.Participants) == 0 {
		return staleHold("withdrawal_cost_unavailable")
	}
	total := p.OtherHolders
	inc := p.IncrementalOtherHolders
	remainingLP := p.EffectiveLP
	seen := map[string]bool{}
	for _, r := range p.Participants {
		id, err := r.Consent.Digest()
		if err != nil || id != r.ID || seen[r.ID] || r.EscrowLP == 0 || r.EscrowLP > remainingLP || r.Payable < 0 || r.MaxCost < 0 || r.IncrementalCost < 0 || r.IncrementalCost > r.MaxCost || r.MaxCost > p.MaxCost-total {
			return staleHold("withdrawal_cost_unavailable")
		}
		seen[r.ID] = true
		remainingLP -= r.EscrowLP
		total += r.MaxCost
		inc += r.IncrementalCost
	}
	if total != p.MaxCost || inc != p.IncrementalCost {
		return staleHold("withdrawal_cost_not_conserved")
	}
	return nil
}

func (p *withdrawalCostPreparation) revalidate(ctx context.Context, message []byte) error {
	if p == nil || p.view == nil || sha256Bytes(message) != p.MessageSHA256 {
		return staleHold("withdrawal_cost_evidence_mismatch")
	}
	slot, _, receipts, err := p.view.readWithReceipts(ctx, p.manifest.vault, nil, p.Slot)
	if err != nil {
		return err
	}
	_, fingerprint, err := decodeConfirmedWithdrawalDemand(p.manifest.vault, receipts)
	if err != nil || fingerprint != p.ReceiptFingerprint || !p.net.valid(time.Now(), slot) {
		return staleHold("withdrawal_generation_changed")
	}
	if p.chain == nil {
		return staleHold("withdrawal_generation_verifier_unavailable")
	}
	reader := earnmax.NewReader(p.chain, nil)
	for _, r := range p.Participants {
		if err := reader.VerifyWithdrawalGeneration(ctx, r.Consent); err != nil {
			return staleHoldFrom(err, "withdrawal_generation_changed")
		}
	}
	slot, err = p.view.slot(ctx)
	if err != nil {
		return err
	}
	if !p.net.valid(time.Now(), slot) {
		return staleHold("withdrawal_cost_evidence_expired")
	}
	return nil
}

func withdrawalCostWait(reason earnmax.WithdrawalWaitReason) error {
	return staleHold("withdrawal_" + string(reason))
}

func withdrawalCostLimits(c earnmax.WithdrawalConsent, spent, reserved, newCost earnmax.USDCRaw) error {
	reference, e1 := strconv.ParseInt(c.ReferenceUSDC, 10, 64)
	floor, e2 := strconv.ParseInt(c.MinClaimUSDC, 10, 64)
	if e1 != nil || e2 != nil || floor < 0 || reference < floor || spent < 0 || reserved < 0 || newCost < 0 {
		return withdrawalCostWait(earnmax.WithdrawalUnknownCost)
	}
	remaining := earnmax.USDCRaw(reference - floor)
	for _, cost := range []earnmax.USDCRaw{spent, reserved, newCost} {
		if cost > remaining {
			return withdrawalCostWait(earnmax.WithdrawalWaitingPrice)
		}
		remaining -= cost
	}
	return nil
}

func (p withdrawalCostPreparation) String() string {
	return fmt.Sprintf("withdrawal-cost:%s:%d", p.MessageSHA256, p.Slot)
}
