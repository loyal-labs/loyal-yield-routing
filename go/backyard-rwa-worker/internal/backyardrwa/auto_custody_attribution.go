package backyardrwa

// Durable shared-PYUSD custody attribution for the candidate AUTO lane.
//
// AUTO, Ethena and primePRIMEPYUSD share one PYUSD debt custody account, so
// observed balances plus a caller-filled strategy key are custody identity,
// never lane ownership. This is the missing durable gate: it proves the
// freshly observed residue belongs to this lane by walking a bounded,
// contiguous chain of FINALIZED RECONCILED operation receipts that touch the
// shared custody, back to a proven zero-start funding or borrow edge for
// this lane.
//
// The proof uses only ACTUAL receipt balances (the canonical reconciled
// accounts evidence `address:owner:mint:authority:before:after` produced by
// ReconcileConfirmedTransaction, sha256-bound per row). Expected effects and
// the row's action bind exact custody/action/identity only — they never
// replace the actual after balances, and ownership is never inferred from
// address equality, quotes, expected minima, or old upper bounds.
//
// Read-only: the Database reader asserts the CURRENT worker lease (identity
// compared, then SQL-asserted inside one coherent read-only repeatable-read
// transaction) and returns ONE bounded UNFILTERED contiguous window of the
// route's reconciled journal — newest first in the journal's own composite
// order, no custody-substring filter, no content filter of any kind — plus
// three chronology-independent route-wide conflict prechecks (bounded
// EXISTS): any unresolved (nonterminal) route record, any unrecognized
// terminal/manual-state record (except failed rows the journal itself proves
// never signed and never broadcast — the legitimate presend rejection), and
// any reconciled record with a malformed ordering identity (NULL or
// nonpositive slot, absent signature or digest). Every route record is
// covered by exactly one of the four reads, so nothing can fall between
// them. The validator strictness-checks every row in the window from the tip
// THROUGH the proven zero-start origin and STOPS there: strict token-account
// history is only required through the origin, and what sorts behind it —
// prior lifecycles, foreign lanes' earlier use, and the route's own
// kamino-initialize row, whose native-balance evidence deliberately uses a
// different schema with no token accounts — is held closed by the route-wide
// prechecks instead. The completeness boundary is exactly the window bound:
// if the window cannot reach a proven zero-start edge, the proof is refused.
// A reconciled row is skipped as inert only when its own strict hash-bound
// evidence names no custody account AND its built expected effects are
// readable; everything unrecognized, unclassifiable, or corrupt is a
// refusal. No schema change, no row rewrites, no store.go/lifecycle.go
// edits.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// sharedCustodyAttributionDepthBound caps how far back one proof may
	// walk. Exhausting the bound without a proven zero-start edge is a
	// refusal, not a pass.
	sharedCustodyAttributionDepthBound = 16

	// sharedCustodyUnresolvedStatuses is the journal's own nonterminal set
	// (the partial one-nonterminal-per-route unique index). Any row in one of
	// these statuses fails the route closed for attribution, whether or not
	// it carries custody evidence.
	sharedCustodyUnresolvedStatuses = `('decided','built','simulated','signed','broadcast_intent','submitted','confirmed','reconciling')`

	// The canonical reconciled evidence envelope, pinned to reconcile.go.
	reconciledEffectsSchema = "loyal-backyard-rwa-reconciled-effects/v1"
	reconciledEffectsSource = "confirmed-transaction-meta"
)

// custodyAttributionRow is the read-only projection of one operation row
// that may possibly have touched the shared custody.
type custodyAttributionRow struct {
	OperationID          string
	StrategyKey          string
	RouteKey             string
	Action               string
	Status               string
	ConfirmationStatus   string
	ConfirmedSlot        int64
	TransactionSignature string
	ReconciliationSHA256 string
	ReconciledEffects    []byte
	ExpectedEffects      []byte
}

// sharedCustodyAttributionEvidence is the coherent read bundle: ONE bounded
// unfiltered journal window (every route row, newest first in the journal's
// composite order) plus two chronology-independent global conflict flags.
// The flags are independent of the window order and bound, so an unresolved
// or unrecognized record can never be skipped by chain chronology (NULL
// slots sort LAST in the composite order, behind a proven zero-start origin)
// or by the window limit.
type sharedCustodyAttributionEvidence struct {
	Rows []custodyAttributionRow
	// Unresolved is the route-wide existence of any nonterminal operation.
	Unresolved bool
	// Unknown is the route-wide existence of any unrecognized
	// terminal/manual-recovery record, EXCEPT failed rows the journal itself
	// proves never signed and never broadcast (the legitimate presend
	// rejection MarkPreBroadcastFailed writes).
	Unknown bool
	// MalformedIdentity is the route-wide existence of any reconciled record
	// whose ordering identity is malformed: NULL or nonpositive confirmed
	// slot, or absent signature or digest. Such a row sorts LAST in the
	// journal composite order and its strict in-walk checks would refuse it,
	// so it must not be hideable behind a proven zero-start origin or beyond
	// the window bound.
	MalformedIdentity bool
}

// sharedCustodyAttributionConfig pins the exact custody identity the chain
// must prove — one address, one mint, one authority, one owner program, one
// lane — and the two reviewed zero-start edge shapes for this lane.
type sharedCustodyAttributionConfig struct {
	Lane     string
	RouteKey string
	Custody  string
	Mint     string
	// Authority and Owner are the pinned token-account authority and owner
	// program of the shared custody (route.Kamino.Vault and the route's debt
	// token program).
	Authority string
	Owner     string
	// Funding edge: the reviewed AUTO->PYUSD swap delivering PYUSD into the
	// shared custody from the lane's own collateral custody.
	FundingEdgeAction string
	FundingEdgeSource string
	// Borrow edge: the reviewed open-route borrow delivering PYUSD into the
	// shared custody from the route's own debt liquidity supply. The full
	// production kaminoBorrowEffects shape — supply source, custody
	// destination, fee receiver, all one mint with the market authority —
	// must bind, so the fee leg is pinned too.
	BorrowEdgeAction      string
	BorrowEdgeSource      string
	BorrowEdgeAuthority   string
	BorrowEdgeFeeReceiver string
	// Delegate is the sole top-level signer every admitted signed wire must
	// carry: the production config pins the checked-in bridge delegate, and
	// the signed-phase current-operation validation reconstructs the
	// persisted wire evidence and validates it against exactly this key.
	Delegate publicKey
}

// autoSharedPYUSDAttributionConfig pins the shared PYUSD debt custody for the
// candidate AUTO lane from route identities only.
func autoSharedPYUSDAttributionConfig(route RuntimeRoute, routeKey string) sharedCustodyAttributionConfig {
	return sharedCustodyAttributionConfig{
		Lane:              route.Lane,
		RouteKey:          routeKey,
		Custody:           route.DebtCustody,
		Mint:              route.Kamino.DebtMint,
		Authority:         route.Kamino.Vault,
		Owner:             route.DebtTokenProgram,
		FundingEdgeAction: string(SwapCollateralToDebtStep),
		FundingEdgeSource: route.CollateralCustody,
		BorrowEdgeAction:  string(OpenRouteStep),
		BorrowEdgeSource:  route.DebtLiquiditySupply,
		// The borrow legs' non-custody accounts are all authorized by the
		// market authority and pay the route's own fee receiver.
		BorrowEdgeAuthority:   route.Kamino.MarketAuthority,
		BorrowEdgeFeeReceiver: route.DebtFeeReceiver,
		// The production delegate: every admitted signed wire must carry it.
		Delegate: mustKey(bridgeDelegate),
	}
}

// reconciledCustodyAccountEffect is one strictly parsed colon-separated
// canonical account row.
type reconciledCustodyAccountEffect struct {
	Address   string
	Owner     string
	Mint      string
	Authority string
	BeforeRaw uint64
	AfterRaw  uint64
}

// sharedCustodyProofStep is one proven link of the chain.
type sharedCustodyProofStep struct {
	OperationID   string
	Signature     string
	Slot          int64
	BeforeRaw     uint64
	AfterRaw      uint64
	EffectsSHA256 string
}

// sharedCustodyProof is the durable attribution result: deterministic given
// the same rows, so a restart reconstructs the identical proof.
type sharedCustodyProof struct {
	ObservedRaw uint64
	Origin      sharedCustodyProofStep
	Steps       []sharedCustodyProofStep
}

// canonicalSlotNumber reports whether a JSON number token is the canonical
// integer encoding json.Marshal produces for int64: digits only, no leading
// zeros, no fraction or exponent.
func canonicalSlotNumber(token json.Number) bool {
	text := string(token)
	if text == "" || (len(text) > 1 && text[0] == '0') {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return true
}

// reconstructReconciledEvidence re-encodes the journal's jsonb text back to
// the ORIGINAL reconcile.go map encoding with exact integer preservation
// (UseNumber, no float64), so the stored sha256 digest of the original
// marshal can be verified without rewriting it. Strict EOF and field
// validation reject trailing data, unknown fields, and mistyped values;
// returnData, when present, is re-typed to the struct field order the
// original marshal used.
func reconstructReconciledEvidence(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var evidence map[string]any
	if err := decoder.Decode(&evidence); err != nil {
		return nil, budgetHold("custody_attribution_malformed_record")
	}
	// Strict EOF: any trailing value or garbage after the object is a
	// different byte stream than the original single-object marshal.
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, budgetHold("custody_attribution_malformed_record")
	}
	for key, value := range evidence {
		switch key {
		case "schema", "source", "signature":
			if _, isString := value.(string); !isString || value.(string) == "" {
				return nil, budgetHold("custody_attribution_malformed_record")
			}
		case "slot":
			number, isNumber := value.(json.Number)
			if !isNumber || !canonicalSlotNumber(number) {
				return nil, budgetHold("custody_attribution_malformed_record")
			}
		case "accounts":
			accounts, isArray := value.([]any)
			if !isArray || len(accounts) == 0 {
				return nil, budgetHold("custody_attribution_malformed_record")
			}
			for _, entry := range accounts {
				if _, isString := entry.(string); !isString {
					return nil, budgetHold("custody_attribution_malformed_record")
				}
			}
		case "returnData":
			fields, isObject := value.(map[string]any)
			if !isObject || len(fields) != 2 {
				return nil, budgetHold("custody_attribution_malformed_record")
			}
			for name, field := range fields {
				if _, isString := field.(string); !isString || (name != "programId" && name != "dataBase64") {
					return nil, budgetHold("custody_attribution_malformed_record")
				}
			}
			evidence[key] = ExpectedReturnData{ProgramID: fields["programId"].(string), DataBase64: fields["dataBase64"].(string)}
		default:
			return nil, budgetHold("custody_attribution_malformed_record")
		}
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return nil, budgetHold("custody_attribution_malformed_record")
	}
	return encoded, nil
}

// parseReconciledCustodyEvidence strictly validates the canonical reconciled
// evidence: schema/source pinned, signature/slot present, every account row
// exactly address:owner:mint:authority:before:after with valid identities
// and no duplicate addresses.
func parseReconciledCustodyEvidence(data []byte, cfg sharedCustodyAttributionConfig) (string, int64, []reconciledCustodyAccountEffect, error) {
	var evidence struct {
		Schema    string   `json:"schema"`
		Source    string   `json:"source"`
		Signature string   `json:"signature"`
		Slot      int64    `json:"slot"`
		Accounts  []string `json:"accounts"`
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		return "", 0, nil, budgetHold("custody_attribution_malformed_record")
	}
	if evidence.Schema != reconciledEffectsSchema || evidence.Source != reconciledEffectsSource ||
		evidence.Signature == "" || evidence.Slot <= 0 {
		return "", 0, nil, budgetHold("custody_attribution_malformed_record")
	}
	if len(evidence.Accounts) == 0 {
		return "", 0, nil, budgetHold("custody_attribution_malformed_record")
	}
	accounts := make([]reconciledCustodyAccountEffect, 0, len(evidence.Accounts))
	seen := make(map[string]struct{}, len(evidence.Accounts))
	for _, encoded := range evidence.Accounts {
		fields := strings.Split(encoded, ":")
		if len(fields) != 6 {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		effect := reconciledCustodyAccountEffect{Address: fields[0], Owner: fields[1], Mint: fields[2], Authority: fields[3]}
		if effect.Address == "" || (effect.Owner != classicTokenProgram && effect.Owner != token2022Program) ||
			effect.Mint == "" || effect.Authority == "" {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		if _, err := decodeBase58PublicKey(effect.Mint); err != nil {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		if _, err := decodeBase58PublicKey(effect.Authority); err != nil {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		before, err := strconv.ParseUint(fields[4], 10, 64)
		if err != nil {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		effect.BeforeRaw = before
		after, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		effect.AfterRaw = after
		if _, duplicate := seen[effect.Address]; duplicate {
			return "", 0, nil, budgetHold("custody_attribution_malformed_record")
		}
		seen[effect.Address] = struct{}{}
		accounts = append(accounts, effect)
	}
	return evidence.Signature, evidence.Slot, accounts, nil
}

// classifyRowCustody resolves the shared custody's actual balances from one
// parsed evidence. A row with an account at the custody address whose mint,
// authority or owner does not match the pinned identity is an unknown
// possible touch and must be rejected, never skipped; only a row with no
// account at the custody address at all can be classified as not a touch.
func classifyRowCustody(accounts []reconciledCustodyAccountEffect, cfg sharedCustodyAttributionConfig) (reconciledCustodyAccountEffect, bool, error) {
	for _, effect := range accounts {
		if effect.Address != cfg.Custody {
			continue
		}
		if effect.Mint != cfg.Mint || effect.Authority != cfg.Authority || effect.Owner != cfg.Owner {
			return reconciledCustodyAccountEffect{}, false, budgetHold("custody_attribution_unknown_record")
		}
		return effect, true, nil
	}
	return reconciledCustodyAccountEffect{}, false, nil
}

// rowBindsExpectedCustody checks the operation's built expected effects
// intend exactly this custody identity — an identity bind only; the chain
// math still uses the actual receipt balances.
func rowBindsExpectedCustody(row custodyAttributionRow, cfg sharedCustodyAttributionConfig) bool {
	expected, err := DecodeExpectedEffects(row.ExpectedEffects)
	if err != nil {
		return false
	}
	for _, effect := range expected.Accounts {
		if effect.Address == cfg.Custody && effect.Mint == cfg.Mint && effect.Authority == cfg.Authority && effect.Owner == cfg.Owner {
			return true
		}
	}
	return false
}

// rowExpectedCustodyTouch reports whether the built expected effects name
// the shared custody ADDRESS at all. An unreadable expected-effects envelope
// is an error, never a silent "no touch": a row whose built intent cannot be
// decoded must not be classified inert just because its strict actual
// receipt also names no custody account.
func rowExpectedCustodyTouch(row custodyAttributionRow, cfg sharedCustodyAttributionConfig) (bool, error) {
	expected, err := DecodeExpectedEffects(row.ExpectedEffects)
	if err != nil {
		return false, err
	}
	for _, effect := range expected.Accounts {
		if effect.Address == cfg.Custody {
			return true, nil
		}
	}
	return false, nil
}

// isNonterminalCustodyStatus reports whether a status is in the journal's
// own nonterminal set (the partial one-nonterminal-per-route index).
func isNonterminalCustodyStatus(status string) bool {
	switch status {
	case "decided", "built", "simulated", "signed", "broadcast_intent", "submitted", "confirmed", "reconciling":
		return true
	}
	return false
}

// sharedCustodySpendRaw reports the positive raw amount the operation's OWN
// built expected effects debit from the pinned shared custody identity
// (owner+mint+authority at the custody address). Zero means the operation
// does not spend the shared custody at all — a NAV report, a credit-only
// funding, or an unrelated touch — so the attribution gate does not apply to
// it. A positive result is always gated, even at zero observed balance.
func sharedCustodySpendRaw(expected ExpectedEffects, cfg sharedCustodyAttributionConfig) uint64 {
	var spend uint64
	for _, effect := range expected.Accounts {
		if effect.Address != cfg.Custody || effect.Mint != cfg.Mint || effect.Authority != cfg.Authority || effect.Owner != cfg.Owner {
			continue
		}
		if effect.AfterRaw < effect.BeforeRaw {
			spend += effect.BeforeRaw - effect.AfterRaw
		}
	}
	return spend
}

// sharedCustodyCurrentOperation is the caller's claim that exactly one
// in-flight operation is the shared-custody spend being admitted, built, or
// broadcast. It is NEVER trusted: the reader validates the persisted row
// inside its own snapshot before applying any exclusion. The expected
// effects are the SAME decoded spend intent the caller is acting on — the
// reader requires them to equal the persisted built effects exactly (the
// same custody at a different amount is not the same spend).
type sharedCustodyCurrentOperation struct {
	OperationID string
	// SignedWireSHA256 and TransactionSignature are the exact persisted wire
	// digest and signature; REQUIRED once the operation is signed and must
	// both be empty before signing.
	SignedWireSHA256     string
	TransactionSignature string
	ExpectedEffects      ExpectedEffects
}

// sharedCustodyCurrentOperationRow is the persisted projection the reader
// validates the claim against.
type sharedCustodyCurrentOperationRow struct {
	Status               string
	StrategyKey          string
	SignedWirePresent    bool
	SignedWire           []byte
	SignedWireSHA256     string
	TransactionSignature string
	MessageSHA256        string
	RecentBlockhash      string
	LastValidBlockHeight int64
	SimulationSlot       int64
	BroadcastIntent      bool
	ExpectedEffects      []byte
}

// signedWirePersistedEvidence rebuilds the exact evidence the production
// signing persistence validates, so the exclusion binds the claimed wire to
// the persisted bytes with the production validator itself: wire digest,
// message digest, recent blockhash, sole-signer signature recovery and
// signature encoding must all match.
func signedWirePersistedEvidence(row sharedCustodyCurrentOperationRow) BuildResult {
	return BuildResult{
		MessageSHA256: row.MessageSHA256, SignedWire: row.SignedWire, SignedWireSHA256: row.SignedWireSHA256,
		TransactionSignature: row.TransactionSignature, RecentBlockhash: row.RecentBlockhash,
		LastValidBlockHeight: row.LastValidBlockHeight, SimulationSlot: row.SimulationSlot,
	}
}

// validateSharedCustodyCurrentOperation refuses every persisted state that
// does not make the operation the route's one SIGNED custody spend of this
// lane: exact route (by the query), exact lane (lanes share route keys),
// signed status with the exact persisted signature the production signing
// persistence wrote — bound to the decoded signed wire's own signature and to
// the hash of the actual bytes — with no broadcast intent. Decided/built/
// simulated rows are NEVER excludable: recordDecisionTx persists only the
// decision evidence (expectedEffects is null and DecodeExpectedEffects
// refuses that state), so no custody walk runs while such a row is current —
// the ownership proof runs BEFORE RecordDecision (no row exists, strict
// gates) and the send proof AFTER signing. The caller's spend intent must
// equal the persisted built effects exactly, and must positively debit the
// shared custody. The one-nonterminal-per-route index alone never authorizes
// an exclusion.
func validateSharedCustodyCurrentOperation(current sharedCustodyCurrentOperation, row sharedCustodyCurrentOperationRow, cfg sharedCustodyAttributionConfig) error {
	if current.OperationID == "" || row.StrategyKey != cfg.Lane {
		return budgetHold("custody_attribution_current_operation_invalid")
	}
	switch row.Status {
	case "signed":
		if !row.SignedWirePresent || len(row.SignedWire) == 0 ||
			row.SignedWireSHA256 == "" || row.SignedWireSHA256 != current.SignedWireSHA256 ||
			row.TransactionSignature == "" || row.TransactionSignature != current.TransactionSignature ||
			row.BroadcastIntent {
			return budgetHold("custody_attribution_current_operation_invalid")
		}
		// The persisted wire must be the complete exact evidence the signing
		// persistence writes — digest, message, blockhash, sole-signer
		// signature — AND carry the pinned delegate as its signer.
		if signedWirePersistedEvidence(row).validateForDelegate(cfg.Delegate) != nil {
			return budgetHold("custody_attribution_current_operation_invalid")
		}
	default:
		// decided/built/simulated (no built effects persisted yet — see
		// above), submitted, broadcast_intent, confirmed, reconciling,
		// failed, manual-recovery and every terminal state: not excludable.
		return budgetHold("custody_attribution_current_operation_invalid")
	}
	if row.BroadcastIntent {
		return budgetHold("custody_attribution_current_operation_invalid")
	}
	persisted, err := DecodeExpectedEffects(row.ExpectedEffects)
	if err != nil {
		return budgetHold("custody_attribution_current_operation_invalid")
	}
	if !reflect.DeepEqual(persisted, current.ExpectedEffects) {
		return budgetHold("custody_attribution_current_operation_invalid")
	}
	if sharedCustodySpendRaw(persisted, cfg) == 0 {
		return budgetHold("custody_attribution_current_operation_invalid")
	}
	return nil
}

// isProvenZeroStartEdge binds the reviewed funding/borrow edge by ACTION and
// EXPECTED-INPUT SHAPE (kind, exact account layout, lane-private source and
// fee identities, custody-as-destination-from-zero) — never by the action
// label alone, and never by indexing past a malformed length. The caller
// still requires the ACTUAL receipt before to equal zero.
func isProvenZeroStartEdge(row custodyAttributionRow, cfg sharedCustodyAttributionConfig) bool {
	expected, err := DecodeExpectedEffects(row.ExpectedEffects)
	if err != nil {
		return false
	}
	custodyFromZero := func(effect ExpectedAccountEffect) bool {
		return effect.Address == cfg.Custody && effect.Mint == cfg.Mint && effect.Authority == cfg.Authority &&
			effect.Owner == cfg.Owner && effect.BeforeRaw == 0
	}
	switch {
	case row.Action == cfg.FundingEdgeAction && expected.Kind == "cross-mint-swap" && len(expected.Accounts) == 2:
		// The reviewed funding swap: the lane's own collateral custody pays,
		// the shared PYUSD custody is the destination funded from zero.
		return expected.Accounts[0].Address == cfg.FundingEdgeSource && custodyFromZero(expected.Accounts[1])
	case row.Action == cfg.BorrowEdgeAction && expected.Kind == "kamino-borrow" && len(expected.Accounts) == 3:
		// The exact production kaminoBorrowEffects shape: [0] the route's
		// own debt liquidity supply pays, [1] the shared PYUSD custody is
		// the destination funded from zero, [2] the route's own fee
		// receiver takes the origination fee — one mint, market-authority
		// signed outside the custody.
		supply, fee := expected.Accounts[0], expected.Accounts[2]
		return supply.Address == cfg.BorrowEdgeSource && supply.Mint == cfg.Mint && supply.Owner == cfg.Owner && supply.Authority == cfg.BorrowEdgeAuthority &&
			custodyFromZero(expected.Accounts[1]) &&
			fee.Address == cfg.BorrowEdgeFeeReceiver && fee.Mint == cfg.Mint && fee.Owner == cfg.Owner && fee.Authority == cfg.BorrowEdgeAuthority
	default:
		return false
	}
}

func custodyAttributionStep(row custodyAttributionRow, signature string, slot int64, effect reconciledCustodyAccountEffect) sharedCustodyProofStep {
	return sharedCustodyProofStep{OperationID: row.OperationID, Signature: signature, Slot: slot, BeforeRaw: effect.BeforeRaw, AfterRaw: effect.AfterRaw, EffectsSHA256: row.ReconciliationSHA256}
}

// validateSharedCustodyAttribution proves the observed shared-custody balance
// belongs to cfg.Lane. The evidence bundle must be the coherent read bundle
// from the lease-scoped reader, observedRaw/observedSlot the FRESH snapshot
// the proof is bound to. BEFORE any chain walking it fails closed on the
// three route-wide prechecks — any unresolved (nonterminal) operation, any
// unrecognized manual/terminal-state record (except proven no-sign/no-
// broadcast presend failures), and any reconciled record with a malformed
// ordering identity — however they sort and however the window bound
// truncates, so such records cannot hide behind a proven zero-start origin
// or beyond the window. The walk then strictness-checks EVERY row of the
// unfiltered window from the tip back THROUGH the origin: foreign routes,
// unrecognized states, NULL receipts, unreadable or custody-intending
// expected effects, malformed canonical evidence, digest mismatches, and
// evidence/journal identity drift refuse the proof at any position of the
// proven segment. That segment — newest custody touch back to the origin —
// additionally refuses foreign lanes, ambiguous same-slot chronology, any
// touch newer than the snapshot slot, unfinalized operations, intent drift,
// balance mismatch at the tip, and continuity gaps, and is complete only at
// a proven before==0 funding or borrow edge for this lane inside the window;
// an unproven walk is a refusal. At the proven edge the walk STOPS: the
// edge's actual before==0 severs older history (prior lifecycles, foreign
// lanes' earlier use, the route's own kamino-initialize row with its
// native-balance evidence) from this proof, and the route-wide gates above
// are what hold it closed. A restart over the same rows reconstructs the
// identical proof.
func validateSharedCustodyAttribution(observedRaw uint64, observedSlot int64, cfg sharedCustodyAttributionConfig, evidence sharedCustodyAttributionEvidence, maxDepth int) (sharedCustodyProof, error) {
	// Precheck 1, route-wide and chronology-independent: any pending or
	// ambiguous operation — with or without custody evidence — means the
	// custody state is not settled. A NULL confirmed_slot sorts LAST in the
	// journal composite order, behind a proven zero-start origin, and could
	// otherwise hide there or beyond the window bound; a route-wide
	// existence flag cannot be hidden by either.
	if evidence.Unresolved {
		return sharedCustodyProof{}, budgetHold("custody_attribution_unresolved_operation")
	}
	// Precheck 2, route-wide and chronology-independent: an unrecognized
	// manual/terminal state has unproven provenance. It holds regardless of
	// its content, slot, or position: the reconciled account list is only the
	// expected-effects projection, so no classification of such a record can
	// prove it never moved the shared custody. Failed rows the journal itself
	// proves never signed and never broadcast are excluded by the reader —
	// the one legitimate presend rejection must not disable cleanup forever.
	if evidence.Unknown {
		return sharedCustodyProof{}, budgetHold("custody_attribution_unknown_record")
	}
	// Precheck 3, route-wide and chronology-independent: a reconciled record
	// whose ordering identity is malformed (NULL or nonpositive confirmed
	// slot, absent signature or digest) sorts LAST in the journal composite
	// order — behind a proven zero-start origin — and could otherwise hide
	// there or beyond the window bound, even though the walk's strict
	// identity bind would have refused it.
	if evidence.MalformedIdentity {
		return sharedCustodyProof{}, budgetHold("custody_attribution_malformed_identity")
	}
	if maxDepth <= 0 || maxDepth > sharedCustodyAttributionDepthBound {
		maxDepth = sharedCustodyAttributionDepthBound
	}
	proof := sharedCustodyProof{ObservedRaw: observedRaw}
	prevBefore := uint64(0)
	prevSlot := int64(0)
	tip := true
	depth := 0
	originFound := false
	for _, row := range evidence.Rows {
		if row.RouteKey != cfg.RouteKey {
			return sharedCustodyProof{}, budgetHold("custody_attribution_foreign_route")
		}
		// Defense in depth for direct callers: an unrecognized state holds at
		// every position of the walk (the reader routes such rows to the
		// independent route-wide precheck).
		if row.Status != "reconciled" && !isNonterminalCustodyStatus(row.Status) {
			return sharedCustodyProof{}, budgetHold("custody_attribution_unknown_record")
		}
		if len(row.ReconciledEffects) == 0 {
			// A reconciled row whose receipt never landed (NULL evidence)
			// cannot be classified; ownership may not be proven by omitting
			// it.
			return sharedCustodyProof{}, budgetHold("custody_attribution_malformed_record")
		}
		canonical, err := reconstructReconciledEvidence(row.ReconciledEffects)
		if err != nil {
			return sharedCustodyProof{}, err
		}
		if sha256Bytes(canonical) != row.ReconciliationSHA256 {
			return sharedCustodyProof{}, budgetHold("custody_attribution_hash_mismatch")
		}
		signature, slot, accounts, err := parseReconciledCustodyEvidence(row.ReconciledEffects, cfg)
		if err != nil {
			return sharedCustodyProof{}, err
		}
		// Bind the receipt identity to the journal's own record.
		if signature != row.TransactionSignature || slot != row.ConfirmedSlot {
			return sharedCustodyProof{}, budgetHold("custody_attribution_identity_mismatch")
		}
		effect, touches, err := classifyRowCustody(accounts, cfg)
		if err != nil {
			return sharedCustodyProof{}, err
		}
		if !touches {
			expectsTouch, err := rowExpectedCustodyTouch(row, cfg)
			if err != nil {
				// The built expected effects are unreadable: the row's
				// custody intent cannot be established, so the row must never
				// be deemed inert — even though its strict actual receipt
				// names no custody account.
				return sharedCustodyProof{}, budgetHold("custody_attribution_malformed_record")
			}
			if expectsTouch {
				// The operation intended a custody touch but the strict
				// actual receipt has no matching custody account: an unknown
				// capital movement that cannot be classified.
				return sharedCustodyProof{}, budgetHold("custody_attribution_unknown_record")
			}
			// The one recognized inert classification: a strict,
			// digest-bound RECONCILED record whose built expected effects and
			// receipt projection name no account at the custody address.
			// This rests on the production build invariant that every
			// custody-moving operation carries its custodies in its expected
			// effects; unrecognized states never reach this skip (precheck
			// 2 holds them unconditionally).
			continue
		}
		// Every participant must be this lane's own finalized reconciled
		// operation: an unfinalized touch may still land and move the
		// balance; a foreign-lane touch means the provenance is not this
		// lane's alone.
		if !originFound {
			depth++
			if depth > maxDepth {
				return sharedCustodyProof{}, budgetHold("custody_attribution_bound_exhausted")
			}
			// Chain chronology: the journal composite order plus strictly
			// decreasing participant slots; two touches in one slot have no
			// provable intra-slot order.
			if !tip && slot >= prevSlot {
				return sharedCustodyProof{}, budgetHold("custody_attribution_ambiguous_order")
			}
			// The chain may never reach past the fresh observation: a custody
			// touch recorded above the snapshot slot means the observed balance
			// no longer describes the chain tip.
			if slot > observedSlot {
				return sharedCustodyProof{}, budgetHold("custody_attribution_snapshot_drift")
			}
			if row.Status != "reconciled" || row.ConfirmationStatus != "finalized" {
				if tip {
					return sharedCustodyProof{}, budgetHold("custody_attribution_unresolved_tip")
				}
				return sharedCustodyProof{}, budgetHold("custody_attribution_unresolved_operation")
			}
			if row.StrategyKey != cfg.Lane {
				return sharedCustodyProof{}, budgetHold("custody_attribution_foreign_lane")
			}
			if !rowBindsExpectedCustody(row, cfg) {
				return sharedCustodyProof{}, budgetHold("custody_attribution_intent_mismatch")
			}
			// The tip's actual after must equal the fresh observed residue
			// exactly; each older link's actual after must equal the newer
			// link's actual before.
			if tip {
				if effect.AfterRaw != observedRaw {
					return sharedCustodyProof{}, budgetHold("custody_attribution_balance_mismatch")
				}
			} else if effect.AfterRaw != prevBefore {
				return sharedCustodyProof{}, budgetHold("custody_attribution_gap")
			}
			proof.Steps = append(proof.Steps, custodyAttributionStep(row, signature, slot, effect))
			// The walk is PROVEN only at a before==0 funding or borrow edge
			// for this lane — including the tip itself, when nothing was
			// spent. At the proven zero-start edge the walk STOPS: strict
			// history is only required THROUGH the origin. Older records —
			// prior lifecycles, foreign lanes' earlier use of the shared
			// account, and the route's own kamino-initialize row (whose
			// native-balance evidence deliberately uses a different schema
			// and no token accounts) — are never parsed here; what sorts
			// behind the edge is held closed by the reader's independent
			// route-wide gates (unresolved, unknown, malformed ordering
			// identity), not by token-account strictness.
			if effect.BeforeRaw == 0 && isProvenZeroStartEdge(row, cfg) {
				proof.Origin = proof.Steps[len(proof.Steps)-1]
				originFound = true
				break
			}
			prevBefore = effect.BeforeRaw
			prevSlot = slot
			tip = false
		}
	}
	if originFound {
		return proof, nil
	}
	if tip {
		return sharedCustodyProof{}, budgetHold("custody_attribution_no_evidence")
	}
	return sharedCustodyProof{}, budgetHold("custody_attribution_origin_unproven")
}

// observeSharedCustodyAttributionEvidence is the read-only bounded reader.
// The passed lease must be THIS worker Database's own current lease (identity
// compared via currentLease), then asserted against the route state row
// inside one coherent read-only repeatable-read transaction that reads
// (a) whether ANY unresolved operation exists on the route key, (b) whether
// ANY route record sits in an unrecognized terminal/manual state — except
// failed rows the journal proves never signed and never broadcast — and (c)
// whether ANY reconciled record has a malformed ordering identity: three
// bounded existence probes, independent of the composite order and of the
// window limit — plus (d) ONE bounded unfiltered contiguous window of the
// route's reconciled journal, newest first in the journal's own composite
// order: no custody-substring filter, no content filter of any kind, every
// strategy key. Every route record is therefore covered by exactly one of
// the four reads, so an unresolved, unrecognized, malformed, or
// capital-moving record cannot be skipped by chain chronology or by LIMIT
// truncation; the window bound is the proof's completeness boundary.
// Conflicting lanes are never filtered out — the validator refuses them. No
// schema change, no row rewrite.
func (d *Database) observeSharedCustodyAttributionEvidence(ctx context.Context, lease RouteLease, cfg sharedCustodyAttributionConfig, limit int, current *sharedCustodyCurrentOperation) (sharedCustodyAttributionEvidence, error) {
	var evidence sharedCustodyAttributionEvidence
	if d == nil || d.pool == nil {
		return evidence, budgetHold("custody_attribution_lease_unavailable")
	}
	held, err := d.currentLease()
	if err != nil {
		return evidence, budgetHold("custody_attribution_lease_unavailable")
	}
	// The reader is scoped to this worker's own lease: a caller may not
	// borrow an arbitrary or stale lease identity to widen the route scope.
	if held.RouteKey != lease.RouteKey || held.Owner != lease.Owner || held.FencingToken != lease.FencingToken {
		return evidence, budgetHold("custody_attribution_lease_unavailable")
	}
	if cfg.Custody == "" || cfg.RouteKey != lease.RouteKey || cfg.Lane == "" || cfg.Mint == "" || cfg.Authority == "" || cfg.Owner == "" {
		return evidence, budgetHold("custody_attribution_no_evidence")
	}
	if limit <= 0 || limit > 256 {
		limit = 64
	}
	tx, err := d.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly, IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return evidence, err
	}
	defer tx.Rollback(ctx)
	// Current-lease assertion inside the same snapshot: the read is only
	// coherent while THIS lease is still the held one.
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, AssertRouteLeaseSQL, lease.RouteKey, lease.Owner, lease.FencingToken).Scan(&expiresAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return evidence, budgetHold("custody_attribution_lease_unavailable")
		}
		return evidence, err
	}
	// The one excludable in-flight operation, validated INSIDE this snapshot
	// against its persisted row. A claim that does not validate is a refusal
	// with no exclusion — never a silent strict read.
	excludeOperationID := ""
	if current != nil {
		var row sharedCustodyCurrentOperationRow
		var effectsText string
		if err := tx.QueryRow(ctx, `SELECT status, COALESCE(strategy_key,''), signed_wire IS NOT NULL,
			COALESCE(signed_wire,''), COALESCE(signed_wire_sha256,''), COALESCE(transaction_signature,''),
			COALESCE(message_sha256,''), COALESCE(recent_blockhash,''), COALESCE(last_valid_block_height,0),
			COALESCE(simulation_slot,0), broadcast_intent_at IS NOT NULL, COALESCE(expected_effects::text,'')
			FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2`,
			current.OperationID, lease.RouteKey).Scan(&row.Status, &row.StrategyKey, &row.SignedWirePresent,
			&row.SignedWire, &row.SignedWireSHA256, &row.TransactionSignature, &row.MessageSHA256,
			&row.RecentBlockhash, &row.LastValidBlockHeight, &row.SimulationSlot, &row.BroadcastIntent,
			&effectsText); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return evidence, budgetHold("custody_attribution_current_operation_invalid")
			}
			return evidence, err
		}
		row.ExpectedEffects = []byte(effectsText)
		if err := validateSharedCustodyCurrentOperation(*current, row, cfg); err != nil {
			return evidence, err
		}
		excludeOperationID = current.OperationID
	}
	// Precheck 1, route-wide existence probe, independent of any ordering or
	// limit: any unresolved (nonterminal) operation on the route, with or
	// without custody evidence — EXCEPT the one validated in-flight operation
	// above (an empty excluded ID matches nothing, so a nil current leaves
	// the gate strict). A NULL confirmed_slot sorts LAST in the
	// journal composite order — behind a proven zero-start origin — and
	// could otherwise hide there or beyond the window bound; an existence
	// flag cannot be hidden by either.
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations
		WHERE route_key=$1 AND status IN `+sharedCustodyUnresolvedStatuses+` AND operation_id <> $2)`,
		lease.RouteKey, excludeOperationID).Scan(&evidence.Unresolved); err != nil {
		return evidence, err
	}
	// Precheck 2, route-wide existence probe, independent of any ordering or
	// limit: ANY route record in an unrecognized terminal/manual state has
	// unproven provenance and holds the route closed regardless of its
	// content, slot, or position — EXCEPT failed rows the journal itself
	// proves never signed and never broadcast: NULL signed wire, wire digest,
	// transaction signature, and broadcast intent, with an explicit failure
	// reason. That is exactly the state MarkPreBroadcastFailed writes for a
	// legitimate presend rejection (from decided/built/simulated, which
	// cannot have produced a signature), and one such rejection must not
	// disable attribution forever. Ambiguous failures carrying signing or
	// broadcast state — including signed wires whose signature was never
	// confirmed — and manual-recovery records still hold.
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations
		WHERE route_key=$1 AND status NOT IN `+sharedCustodyUnresolvedStatuses+` AND status <> 'reconciled'
		AND NOT (status='failed' AND signed_wire IS NULL AND COALESCE(signed_wire_sha256,'')=''
			AND COALESCE(transaction_signature,'')='' AND broadcast_intent_at IS NULL
			AND COALESCE(recovery_reason,'')<>''))`, lease.RouteKey).Scan(&evidence.Unknown); err != nil {
		return evidence, err
	}
	// Precheck 3, route-wide existence probe, independent of any ordering or
	// limit: a reconciled record with a malformed ordering identity — NULL or
	// nonpositive confirmed slot, absent signature or digest — would fail the
	// walk's strict identity bind if reached, but NULL slots sort LAST in the
	// journal composite order, so such a record can hide behind a proven
	// zero-start origin or beyond the window bound. This gate cannot be
	// hidden by either.
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_operations
		WHERE route_key=$1 AND status='reconciled'
		AND (confirmed_slot IS NULL OR confirmed_slot <= 0
			OR COALESCE(transaction_signature,'')='' OR COALESCE(reconciliation_sha256,'')=''))`, lease.RouteKey).Scan(&evidence.MalformedIdentity); err != nil {
		return evidence, err
	}
	// The ONE bounded unfiltered contiguous window of the route's reconciled
	// journal, newest first in the journal's own composite order, all
	// strategy keys. Status is pinned to 'reconciled' because the two
	// route-wide probes above own every other state; there is no
	// custody-substring filter and no other content filter, so every
	// reconciled row from the tip back — inert or not, any lane — is
	// strictness-checked by the validator and a malformed or capital-moving
	// record cannot hide behind an absent substring. The window bound is the
	// completeness boundary: a chain that cannot reach a proven zero-start
	// edge inside it is refused, never passed.
	rows, err := tx.Query(ctx, `SELECT operation_id, COALESCE(strategy_key,''), route_key, COALESCE(action,''), status,
		COALESCE(confirmation_status,''), COALESCE(confirmed_slot,0), COALESCE(transaction_signature,''),
		COALESCE(reconciliation_sha256,''), COALESCE(reconciled_effects::text,''), COALESCE(expected_effects::text,'')
		FROM loyal_yield.multiply_operations
		WHERE route_key=$1 AND status='reconciled'
		ORDER BY confirmed_slot DESC NULLS LAST, updated_at DESC, operation_id COLLATE "C" DESC LIMIT $2`, lease.RouteKey, limit)
	if err != nil {
		return evidence, err
	}
	for rows.Next() {
		var row custodyAttributionRow
		if err := rows.Scan(&row.OperationID, &row.StrategyKey, &row.RouteKey, &row.Action, &row.Status,
			&row.ConfirmationStatus, &row.ConfirmedSlot, &row.TransactionSignature,
			&row.ReconciliationSHA256, &row.ReconciledEffects, &row.ExpectedEffects); err != nil {
			rows.Close()
			return evidence, err
		}
		evidence.Rows = append(evidence.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return evidence, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return evidence, err
	}
	return evidence, nil
}
