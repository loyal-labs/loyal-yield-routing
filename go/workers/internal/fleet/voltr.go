package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"

	solana "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// The Backyard Voltr four-market manager route, ported from loyal-actions
// voltr_kamino.rs. voltr_route.json is the part of the pinned catalog
// docs/evidence/backyard-voltr-four-market/runtime-policy-catalog-v2.json the
// route needs; voltr_test proves it against that file and its Rust pins.

//go:embed voltr_route.json
var voltrRouteJSON []byte

// VoltrKind is the execution_plan kind of a Voltr manager opportunity.
const VoltrKind = "voltr_kamino"

const (
	voltrRouteID                  = "loyal-backyard-four-market-usdc-v1"
	voltrRouteSpecSHA256          = "df6547aeaba99f6bf32a0f56d63c50d30f84d7dc1d3df801266b97bd9811e8f4"
	voltrCatalogFileSHA256        = "94cba2580b915cf9c93a7bd853701cc1acaafc9b0ab6c83492afbae3cfc209df"
	voltrWithdrawalWaitSeconds    = 600
	voltrOptimizationInterval     = 3600
	voltrSafetyBufferRaw          = 0
	voltrIdleATA                  = "9LHpTxtFDYb8xJAruX9uTrceohFms2KyRvkXREj3iV9P"
	voltrLPMint                   = "dbQkLsUYE7ADHHv8XEottANAa773K4xM4nyPjVdutka"
	VoltrLookupTable              = "HSmmBwB7ZRWEsWf4q47w65hXfmqNrfP67KDtpuVrHK7T"
	voltrLookupTableAuthority     = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ"
	VoltrLookupTableAddressCount  = 185
	VoltrLookupTableOrderedSHA256 = "901173cf1cc0bafa9152c66425eb5a4c05819cbdfa742bc9c489d4fa167157c5"
	voltrVaultProgram             = "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"
	voltrKaminoAdaptorProgram     = "to6Eti9CsC5FGkAtqiPphvKD2hiQiLsS8zWiDBqBPKR"
)

var voltrStrategyIDs = [4]string{"main", "onre", "prime", "maple"}

type voltrInstruction struct {
	Policy   string      `json:"policy"`
	Program  string      `json:"program"`
	Data     string      `json:"data"`
	Accounts [][2]string `json:"accounts"`
}

// VoltrStrategy is one Kamino market the Voltr vault allocates to.
type VoltrStrategy struct {
	ID              string           `json:"id"`
	Reserve         string           `json:"reserve"`
	LendingMarket   string           `json:"lendingMarket"`
	StrategyReceipt string           `json:"strategyReceipt"`
	Deposit         voltrInstruction `json:"deposit"`
	Withdraw        voltrInstruction `json:"withdraw"`
}

// VoltrRoute is the embedded, validated route bundle.
type VoltrRoute struct {
	Cluster         string          `json:"cluster"`
	Settings        string          `json:"settings"`
	Manager         string          `json:"manager"`
	Guardian        string          `json:"guardian"`
	Vault           string          `json:"vault"`
	VaultIndex      uint8           `json:"vaultIndex"`
	IdleAuthority   string          `json:"idleAuthority"`
	MaxOperationRaw uint64          `json:"maxOperationRaw"`
	Strategies      []VoltrStrategy `json:"strategies"`
	// BundleSHA256 is Rust's route_bundle_sha256.
	BundleSHA256 string `json:"-"`
}

// LoadVoltrRoute decodes the embedded bundle and derives its identity hash.
func LoadVoltrRoute() (VoltrRoute, error) {
	var r VoltrRoute
	if err := json.Unmarshal(voltrRouteJSON, &r); err != nil {
		return r, err
	}
	if len(r.Strategies) != len(voltrStrategyIDs) || r.MaxOperationRaw == 0 || r.Guardian == "" || r.Vault == "" {
		return r, errors.New("voltr route bundle is incomplete")
	}
	for i, s := range r.Strategies {
		for _, op := range []voltrInstruction{s.Deposit, s.Withdraw} {
			if data, err := base64.StdEncoding.DecodeString(op.Data); err != nil || len(data) != 30 {
				return r, errors.New("voltr manager instruction is not the canonical 30 bytes")
			}
		}
		if s.ID != voltrStrategyIDs[i] {
			return r, errors.New("voltr strategies are not in canonical order")
		}
	}
	r.BundleSHA256 = sha256Hex(fmt.Sprintf("%s:%s:%s:%d:%d:%d:%s:%s:%s:%s:%s:%s:%d", voltrCatalogFileSHA256, voltrRouteID, voltrRouteSpecSHA256,
		voltrWithdrawalWaitSeconds, voltrOptimizationInterval, voltrSafetyBufferRaw, voltrIdleATA, voltrLPMint, VoltrLookupTable,
		voltrLookupTableAuthority, VoltrLookupTableOrderedSHA256, r.Manager, r.MaxOperationRaw))
	return r, nil
}

// StrategyIndex is a strategy's position in canonical Main/OnRe/Prime/Maple order.
func (r VoltrRoute) StrategyIndex(id string) (int, bool) {
	for i, s := range voltrStrategyIDs {
		if s == id {
			return i, true
		}
	}
	return 0, false
}

func (r VoltrRoute) instruction(strategy int, operation string) (voltrInstruction, error) {
	switch operation {
	case "deposit":
		return r.Strategies[strategy].Deposit, nil
	case "withdraw":
		return r.Strategies[strategy].Withdraw, nil
	}
	return voltrInstruction{}, fmt.Errorf("voltr operation %q is not admitted", operation)
}

// ManagerInstruction is Rust's manager_instruction: the canonical inner
// instruction with its amount, wrapped in the guardian's Squads policy.
func (r VoltrRoute) ManagerInstruction(strategy int, operation string, amount uint64) (RouteInstruction, error) {
	if amount == 0 || amount > r.MaxOperationRaw {
		return RouteInstruction{}, fmt.Errorf("voltr amount %d outside 1..%d", amount, r.MaxOperationRaw)
	}
	ix, err := r.instruction(strategy, operation)
	if err != nil {
		return RouteInstruction{}, err
	}
	data, _ := base64.StdEncoding.DecodeString(ix.Data)
	binary.LittleEndian.PutUint64(data[8:16], amount)
	inner := RouteInstruction{Step: "voltr_" + operation, Program: ix.Program, Data: data}
	for _, a := range ix.Accounts {
		inner.Accounts = append(inner.Accounts, InstructionAccount{Address: a[0], Signer: strings.Contains(a[1], "s"), Writable: strings.Contains(a[1], "w")})
	}
	return wrapSquadsPolicy(ix.Policy, r.Guardian, r.VaultIndex, []uint8{0}, []RouteInstruction{inner})
}

// RequirementsFingerprint is Rust's requirements_fingerprint.
func (r VoltrRoute) RequirementsFingerprint(strategy int, operation string) (string, error) {
	wrapped, err := r.ManagerInstruction(strategy, operation, 1)
	if err != nil {
		return "", err
	}
	addresses := []string{}
	for _, a := range wrapped.Accounts {
		addresses = append(addresses, a.Address)
	}
	sort.Strings(addresses)
	unique := addresses[:0]
	for i, a := range addresses {
		if i == 0 || a != addresses[i-1] {
			unique = append(unique, a)
		}
	}
	return sha256Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s", r.BundleSHA256, voltrStrategyIDs[strategy], operation, VoltrLookupTable, VoltrLookupTableOrderedSHA256, strings.Join(unique, ":"))), nil
}

// IntentSHA256 is Rust's manager_intent_sha256.
func (r VoltrRoute) IntentSHA256(strategy int, operation string, amount uint64, slot int64, receipts, state, addresses string) string {
	return sha256Hex(fmt.Sprintf("%s:%s:%s:%d:%d:%s:%s:%s", r.BundleSHA256, voltrStrategyIDs[strategy], operation, amount, slot, receipts, state, addresses))
}

// VoltrLookupTableFromChain verifies the pinned ALT exactly as the Rust
// worker does and returns its ordered members for compilation.
func VoltrLookupTableFromChain(account *chain.Account, slot int64) (LookupTable, error) {
	if account == nil || account.Owner.String() != altProgram || len(account.Data) < 56 || (len(account.Data)-56)%32 != 0 || binary.LittleEndian.Uint64(account.Data[4:12]) != ^uint64(0) || account.Data[21] != 1 || encodeBase58(account.Data[22:54]) != voltrLookupTableAuthority {
		return LookupTable{}, errors.New("voltr pinned ALT owner, authority or activation drifted")
	}
	t := LookupTable{Address: account.Key.String(), Active: true, LastVerifiedSlot: slot}
	h := sha256.New()
	for i := 56; i < len(account.Data); i += 32 {
		address := encodeBase58(account.Data[i : i+32])
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(address)))
		h.Write(n[:])
		h.Write([]byte(address))
		t.Addresses = append(t.Addresses, address)
	}
	if hex.EncodeToString(h.Sum(nil)) != VoltrLookupTableOrderedSHA256 || len(t.Addresses) != VoltrLookupTableAddressCount {
		return LookupTable{}, errors.New("voltr pinned ALT ordered addresses drifted")
	}
	return t, nil
}

// CompileVoltr compiles the guardian-paid v0 manager transaction against the
// pinned ALT, unsigned.
func CompileVoltr(guardian, blockhash string, instruction RouteInstruction, table LookupTable) (PreparedTransaction, error) {
	tx, _, err := compileV0Transaction(guardian, blockhash, []RouteInstruction{instruction}, []LookupTable{table}, 0, 0)
	if err != nil {
		return tx, err
	}
	if tx.PacketBytes > SolanaPacketLimit {
		return tx, fmt.Errorf("voltr packet is %d bytes", tx.PacketBytes)
	}
	return tx, nil
}

// VoltrObservation is one confirmed view of the vault, its idle account, its
// four strategy receipts and its complete withdrawal-receipt set
// (voltr_observation.rs).
type VoltrObservation struct {
	ContextSlot    int64
	TotalValueRaw  uint64
	IdleRaw        uint64
	PositionsRaw   [4]uint64
	PendingRaw     uint64
	EarliestRedeem uint64 // earliest receipt withdrawable_from_ts, 0 when none
	Receipts       string // receipt_set_fingerprint
	State          string // protected_state_sha256
	Addresses      string // protected_address_set_sha256
}

type voltrReceipt struct {
	address  [32]byte
	gen      string
	upper    uint64
	redeemTS uint64
}

var (
	voltrReceiptDiscriminator  = []byte{0xcb, 0x51, 0xdf, 0x8d, 0xaf, 0x6c, 0x65, 0x72}
	voltrStrategyDiscriminator = []byte{51, 8, 192, 253, 115, 78, 112, 214}
	voltrVaultDiscriminator    = []byte{211, 8, 232, 43, 2, 152, 117, 119}
)

// ObserveVoltr brackets the account snapshot with two receipt scans; a
// receipt-set change during the read fails closed.
func ObserveVoltr(ctx context.Context, c *chain.Client, r VoltrRoute, minSlot int64) (VoltrObservation, error) {
	var o VoltrObservation
	if minSlot <= 0 {
		return o, errors.New("voltr observation requires a minimum slot")
	}
	scanSlot, before, err := scanVoltrReceipts(ctx, c, r, minSlot)
	if err != nil {
		return o, err
	}
	addresses := []string{r.Vault, voltrIdleATA}
	for _, s := range r.Strategies {
		addresses = append(addresses, s.StrategyReceipt)
	}
	slot, accounts, err := ReadAccounts(ctx, c, addresses, rpc.CommitmentConfirmed, scanSlot)
	if err != nil {
		return o, err
	}
	if slices.Contains(accounts, nil) {
		return o, errors.New("voltr vault, idle account or strategy receipt is absent")
	}
	vault, idle := accounts[0].Data, accounts[1].Data
	if accounts[0].Owner.String() != voltrVaultProgram || len(vault) != 928 || !bytes.Equal(vault[:8], voltrVaultDiscriminator) || encodeBase58(vault[104:136]) != USDCMint ||
		encodeBase58(vault[368:400]) != r.Manager || binary.LittleEndian.Uint64(vault[456:464]) != voltrWithdrawalWaitSeconds || encodeBase58(vault[136:168]) != voltrIdleATA {
		return o, errors.New("voltr vault owner, layout, manager, asset or withdrawal wait drifted")
	}
	if accounts[1].Owner.String() != tokenProgram || len(idle) != 165 || idle[108] != 1 || encodeBase58(idle[:32]) != USDCMint || encodeBase58(idle[32:64]) != r.IdleAuthority {
		return o, errors.New("voltr idle ATA mint, owner or token program drifted")
	}
	o.ContextSlot, o.TotalValueRaw, o.IdleRaw = slot, binary.LittleEndian.Uint64(vault[168:176]), binary.LittleEndian.Uint64(idle[64:72])
	positions := []string{}
	sum := uint64(0)
	for i, s := range r.Strategies {
		a := accounts[i+2]
		if a.Owner.String() != voltrVaultProgram || len(a.Data) != 192 || !bytes.Equal(a.Data[:8], voltrStrategyDiscriminator) || encodeBase58(a.Data[8:40]) != r.Vault ||
			encodeBase58(a.Data[40:72]) != s.Reserve || encodeBase58(a.Data[72:104]) != voltrKaminoAdaptorProgram || a.Data[120] != 1 || !allZero(a.Data[123:]) {
			return o, errors.New("voltr strategy receipt set drifted")
		}
		o.PositionsRaw[i] = binary.LittleEndian.Uint64(a.Data[104:112])
		if sum+o.PositionsRaw[i] < sum {
			return o, errors.New("voltr position total overflows")
		}
		sum += o.PositionsRaw[i]
		positions = append(positions, fmt.Sprintf("%s:%s:%d:%s", s.ID, s.StrategyReceipt, o.PositionsRaw[i], sha256Hex(string(a.Data))))
	}
	if o.IdleRaw+sum < o.IdleRaw || o.IdleRaw+sum != o.TotalValueRaw {
		return o, errors.New("voltr idle plus positions does not equal vault total value")
	}
	_, after, err := scanVoltrReceipts(ctx, c, r, slot)
	if err != nil {
		return o, err
	}
	if len(after) != len(before) {
		return o, errors.New("voltr receipt set changed during the snapshot")
	}
	gens, members := []string{}, []string{r.Vault}
	for i, receipt := range after {
		if receipt.address != before[i].address || receipt.gen != before[i].gen || i > 0 && receipt.address == after[i-1].address {
			return o, errors.New("voltr receipt set changed during the snapshot")
		}
		if o.PendingRaw+receipt.upper < o.PendingRaw {
			return o, errors.New("voltr pending withdrawals overflow")
		}
		o.PendingRaw += receipt.upper
		if o.EarliestRedeem == 0 || receipt.redeemTS < o.EarliestRedeem {
			o.EarliestRedeem = receipt.redeemTS
		}
		gens = append(gens, receipt.gen)
		members = append(members, encodeBase58(receipt.address[:]))
	}
	o.Receipts = sha256Hex(strings.Join(append([]string{r.Vault}, gens...), ":"))
	o.State = sha256Hex(fmt.Sprintf("%s:%d:%d:%d:%s:%s", r.Vault, slot, o.TotalValueRaw, o.IdleRaw, strings.Join(positions, ":"), strings.Join(gens, ":")))
	for _, s := range r.Strategies {
		members = append(members, s.StrategyReceipt)
	}
	sort.Strings(members)
	unique := members[:0]
	for i, m := range members {
		if i == 0 || m != members[i-1] {
			unique = append(unique, m)
		}
	}
	o.Addresses = sha256Hex(strings.Join(unique, ":"))
	return o, nil
}

func scanVoltrReceipts(ctx context.Context, c *chain.Client, r VoltrRoute, minSlot int64) (int64, []voltrReceipt, error) {
	vault, err := solana.PublicKeyFromBase58(r.Vault)
	if err != nil {
		return 0, nil, err
	}
	filters := []rpc.RPCFilter{{Memcmp: &rpc.RPCFilterMemcmp{Offset: 0, Bytes: voltrReceiptDiscriminator}}, {Memcmp: &rpc.RPCFilterMemcmp{Offset: 8, Bytes: vault[:]}}}
	slot, accounts, err := c.ProgramAccounts(ctx, solana.MustPublicKeyFromBase58(voltrVaultProgram), filters, rpc.CommitmentConfirmed, uint64(minSlot))
	if err != nil {
		return 0, nil, err
	}
	receipts := make([]voltrReceipt, 0, len(accounts))
	for _, v := range accounts {
		data := v.Data
		if v.Owner.String() != voltrVaultProgram || len(data) != 112 || !bytes.Equal(data[:8], voltrReceiptDiscriminator) || !allZero(data[106:]) || encodeBase58(data[8:40]) != r.Vault {
			return 0, nil, errors.New("invalid voltr withdrawal receipt")
		}
		lp, ts := binary.LittleEndian.Uint64(data[72:80]), binary.LittleEndian.Uint64(data[96:104])
		bits := new(big.Int).SetBytes(reverse(data[80:96]))
		if lp == 0 || bits.Sign() == 0 || ts == 0 || data[105] != 0 {
			return 0, nil, errors.New("invalid voltr withdrawal receipt amount, deadline or version")
		}
		upper := new(big.Int).Rsh(new(big.Int).Add(bits, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 48), big.NewInt(1))), 48)
		if !upper.IsUint64() {
			return 0, nil, errors.New("voltr withdrawal receipt overflows")
		}
		gen := sha256Hex(fmt.Sprintf("%s:%s:%s:%d:%s:%d:%d:%d:%s", v.Key, r.Vault, encodeBase58(data[40:72]), lp, bits.String(), ts, data[104], data[105], sha256Hex(string(data))))
		receipts = append(receipts, voltrReceipt{address: v.Key, gen: gen, upper: upper.Uint64(), redeemTS: ts})
	}
	// Rust sorts receipts by Pubkey, which orders by raw bytes.
	sort.Slice(receipts, func(i, j int) bool { return bytes.Compare(receipts[i].address[:], receipts[j].address[:]) < 0 })
	return int64(slot), receipts, nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func reverse(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}
