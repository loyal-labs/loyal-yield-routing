package ata

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/laserstream-worker/internal/watch"
	"github.com/mr-tron/base58"
)

const (
	laserStreamSource = "laserstream_grpc"
	rpcSeedSource     = "rpc_seed"
	commitment        = "confirmed"
	usdcMint          = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

type Handler struct {
	pool    *pgxpool.Pool
	schema  string
	mu      sync.RWMutex
	targets map[string]watch.ATATarget
	rpc     *solanarpc.Client
}

func NewHandler(pool *pgxpool.Pool, streamName string, rpc *solanarpc.Client) *Handler {
	schema := "loyal_prod"
	if streamName == "staging" {
		schema = "loyal_staging"
	}
	return &Handler{pool: pool, schema: schema, targets: make(map[string]watch.ATATarget), rpc: rpc}
}

func (h *Handler) SetTargets(targets map[string]watch.ATATarget) {
	copy := make(map[string]watch.ATATarget, len(targets))
	for key, value := range targets {
		copy[key] = value
	}
	h.mu.Lock()
	h.targets = copy
	h.mu.Unlock()
}

type Outcome struct {
	Slot     uint64
	Inserted bool
	EventID  int64
}

type observation struct {
	target    watch.ATATarget
	pubkey    string
	lamports  uint64
	amount    uint64
	owner     *string
	mint      string
	slot      uint64
	source    string
	signature *string
	data      []byte
	received  time.Time
}

func (h *Handler) HandleAccount(ctx context.Context, update *pb.SubscribeUpdate) (Outcome, error) {
	accountUpdate := update.GetAccount()
	if accountUpdate == nil || accountUpdate.GetAccount() == nil {
		return Outcome{}, fmt.Errorf("ATA filter update omitted account payload")
	}
	account := accountUpdate.GetAccount()
	pubkey, err := publicKey(account.GetPubkey())
	if err != nil {
		return Outcome{}, fmt.Errorf("decode ATA pubkey: %w", err)
	}
	target, ok := h.target(pubkey)
	if !ok {
		return Outcome{Slot: accountUpdate.GetSlot()}, fmt.Errorf("ATA update for unowned account %s", pubkey)
	}
	var signature *string
	if len(account.GetTxnSignature()) > 0 {
		value := base58.Encode(account.GetTxnSignature())
		signature = &value
	}
	observed, err := decodeObservation(target, pubkey, account.GetLamports(), account.GetOwner(), account.GetData(), accountUpdate.GetSlot(), laserStreamSource, signature, time.Now().UTC())
	if err != nil {
		return h.recheck(ctx, target, accountUpdate.GetSlot(), err)
	}
	return h.persist(ctx, observed)
}

func (h *Handler) Seed(ctx context.Context) (uint64, error) {
	h.mu.RLock()
	targets := make([]watch.ATATarget, 0, len(h.targets))
	for _, target := range h.targets {
		targets = append(targets, target)
	}
	h.mu.RUnlock()
	var minimum uint64
	for start := 0; start < len(targets); start += 100 {
		end := start + 100
		if end > len(targets) {
			end = len(targets)
		}
		addresses := make([]string, end-start)
		for index := start; index < end; index++ {
			addresses[index-start] = targets[index].WalletATA
		}
		response, err := h.rpc.MultipleAccounts(ctx, addresses, commitment, nil)
		if err != nil {
			return 0, fmt.Errorf("seed ATA accounts: %w", err)
		}
		if minimum == 0 || response.Slot < minimum {
			minimum = response.Slot
		}
		for index, account := range response.Accounts {
			target := targets[start+index]
			if account == nil {
				empty := sha256.Sum256([]byte("missing:" + target.WalletATA))
				observed := observation{target: target, pubkey: target.WalletATA, amount: 0, mint: target.Mint, slot: response.Slot, source: rpcSeedSource, data: empty[:], received: time.Now().UTC()}
				if _, err := h.persist(ctx, observed); err != nil {
					return 0, err
				}
				continue
			}
			owner, err := solana.PublicKeyFromBase58(account.Owner)
			if err != nil {
				return 0, err
			}
			observed, err := decodeObservation(target, target.WalletATA, account.Lamports, owner[:], account.Data, response.Slot, rpcSeedSource, nil, time.Now().UTC())
			if err != nil {
				// Existing-but-invalid token accounts have no routeable USDC; settle
				// the prior balance to zero with the exact RPC evidence.
				observed = observation{target: target, pubkey: target.WalletATA, lamports: account.Lamports, amount: 0, mint: target.Mint, slot: response.Slot, source: rpcSeedSource, data: account.Data, received: time.Now().UTC()}
			}
			if _, err := h.persist(ctx, observed); err != nil {
				return 0, err
			}
		}
	}
	return minimum, nil
}

func decodeObservation(target watch.ATATarget, pubkey string, lamports uint64, ownerBytes, data []byte, slot uint64, source string, signature *string, received time.Time) (observation, error) {
	if slot == 0 || slot > math.MaxInt64 {
		return observation{}, fmt.Errorf("ATA slot is invalid")
	}
	owner, err := publicKey(ownerBytes)
	if err != nil {
		return observation{}, fmt.Errorf("decode ATA owner program: %w", err)
	}
	if lamports == 0 {
		return observation{target: target, pubkey: pubkey, amount: 0, mint: target.Mint, slot: slot, source: source, signature: signature, data: data, received: received}, nil
	}
	if owner != solana.TokenProgramID.String() {
		return observation{}, fmt.Errorf("ATA %s owner is %s, expected SPL Token", pubkey, owner)
	}
	if len(data) < 72 {
		return observation{}, fmt.Errorf("ATA %s data is %d bytes, expected at least 72", pubkey, len(data))
	}
	mint, err := publicKey(data[:32])
	if err != nil {
		return observation{}, err
	}
	tokenOwner, err := publicKey(data[32:64])
	if err != nil {
		return observation{}, err
	}
	if mint != usdcMint {
		return observation{}, fmt.Errorf("ATA %s mint is %s, expected USDC", pubkey, mint)
	}
	amount := binary.LittleEndian.Uint64(data[64:72])
	if amount > math.MaxInt64 {
		return observation{}, fmt.Errorf("ATA amount exceeds PostgreSQL BIGINT")
	}
	return observation{target: target, pubkey: pubkey, lamports: lamports, amount: amount, owner: &tokenOwner, mint: mint, slot: slot, source: source, signature: signature, data: data, received: received}, nil
}

func (h *Handler) recheck(ctx context.Context, target watch.ATATarget, minimumSlot uint64, streamError error) (Outcome, error) {
	response, err := h.rpc.MultipleAccounts(ctx, []string{target.WalletATA}, commitment, &minimumSlot)
	if err != nil {
		return Outcome{}, fmt.Errorf("ATA stream evidence was invalid (%v) and confirmed recheck failed: %w", streamError, err)
	}
	if response.Slot < minimumSlot {
		return Outcome{}, fmt.Errorf("ATA recheck context slot %d is below stream slot %d", response.Slot, minimumSlot)
	}
	account := response.Accounts[0]
	if account == nil {
		evidence := sha256.Sum256([]byte("missing:" + target.WalletATA))
		return h.persist(ctx, observation{target: target, pubkey: target.WalletATA, amount: 0, mint: target.Mint, slot: response.Slot, source: "rpc_recheck", data: evidence[:], received: time.Now().UTC()})
	}
	owner, err := solana.PublicKeyFromBase58(account.Owner)
	if err != nil {
		return Outcome{}, err
	}
	observed, err := decodeObservation(target, target.WalletATA, account.Lamports, owner[:], account.Data, response.Slot, "rpc_recheck", nil, time.Now().UTC())
	if err != nil {
		// A confirmed wrong-owner or wrong-mint account proves there is no
		// routeable USDC at this address, so settle the target to zero.
		observed = observation{target: target, pubkey: target.WalletATA, lamports: account.Lamports, amount: 0, mint: target.Mint, slot: response.Slot, source: "rpc_recheck", data: account.Data, received: time.Now().UTC()}
	}
	return h.persist(ctx, observed)
}

func (h *Handler) persist(ctx context.Context, observed observation) (Outcome, error) {
	hashBytes := sha256.Sum256(observed.data)
	hash := hex.EncodeToString(hashBytes[:])
	dedupeInput := commitment + ":" + observed.pubkey + ":" + fmt.Sprint(observed.slot) + ":" + hash
	dedupeBytes := sha256.Sum256([]byte(dedupeInput))
	dedupe := hex.EncodeToString(dedupeBytes[:])
	rawBase64 := base64.StdEncoding.EncodeToString(observed.data)
	evidence, err := json.Marshal(map[string]any{
		"lamports": observed.lamports, "account_data_hash": hash,
		"txn_signature": observed.signature, "raw_account_data_base64": rawBase64,
		"source": observed.source, "wallet": observed.target.Wallet,
		"wallet_usdc_ata": observed.target.WalletATA, "vault_pubkey": observed.target.Vault,
		"vault_usdc_ata": observed.target.VaultATA,
	})
	if err != nil {
		return Outcome{}, err
	}
	sequence := h.schema + ".balance_sweep_wallet_ata_observation_event_id_seq"
	query := fmt.Sprintf(`
		WITH candidate AS (SELECT nextval('%s'::regclass) AS event_id), claimed AS (
			INSERT INTO %s.balance_sweep_wallet_ata_observation_dedupe
				(dedupe_key,event_id,source_commitment,wallet_usdc_ata,slot,account_data_hash)
			SELECT $1,candidate.event_id,$2,$3,$4,$5 FROM candidate
			ON CONFLICT (dedupe_key) DO NOTHING RETURNING event_id), inserted AS (
			INSERT INTO %s.balance_sweep_wallet_ata_observations
				(event_id,cluster,target_id,wallet,wallet_usdc_ata,vault_pubkey,vault_usdc_ata,
				 amount_raw,owner,mint,slot,observed_at,source,source_commitment,txn_signature,
				 account_data_hash,raw_account_data_base64,raw_evidence,received_at)
			SELECT event_id,$6,$7,$8,$3,$9,$10,$11,$12,$13,$4,$14,$15,$2,$16,$5,$17,$18,$14 FROM claimed
			RETURNING event_id)
		SELECT event_id,true FROM inserted UNION ALL
		SELECT event_id,false FROM %s.balance_sweep_wallet_ata_observation_dedupe
		WHERE dedupe_key=$1 AND NOT EXISTS(SELECT 1 FROM inserted) LIMIT 1`, sequence, h.schema, h.schema, h.schema)
	outcome := Outcome{Slot: observed.slot}
	if err := h.pool.QueryRow(ctx, query, dedupe, commitment, observed.pubkey, int64(observed.slot), hash, observed.target.Cluster, observed.target.ID, observed.target.Wallet, observed.target.Vault, observed.target.VaultATA, int64(observed.amount), observed.owner, observed.mint, observed.received, observed.source, observed.signature, rawBase64, evidence).Scan(&outcome.EventID, &outcome.Inserted); err != nil {
		return Outcome{}, fmt.Errorf("persist ATA observation: %w", err)
	}
	return outcome, nil
}

func (h *Handler) target(pubkey string) (watch.ATATarget, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	target, ok := h.targets[pubkey]
	return target, ok
}

func publicKey(bytes []byte) (string, error) {
	if len(bytes) != 32 {
		return "", fmt.Errorf("public key has %d bytes", len(bytes))
	}
	var key solana.PublicKey
	copy(key[:], bytes)
	return key.String(), nil
}
