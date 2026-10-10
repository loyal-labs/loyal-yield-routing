package ata

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sync"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	workersdb "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
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
	rpc     *chain.Client
}

func NewHandler(pool *pgxpool.Pool, rpc *chain.Client) *Handler {
	return &Handler{pool: pool, schema: "loyal_prod", targets: make(map[string]watch.ATATarget), rpc: rpc}
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
		return Outcome{}, err
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
		addresses := make([]solana.PublicKey, end-start)
		for index := start; index < end; index++ {
			key, err := solana.PublicKeyFromBase58(targets[index].WalletATA)
			if err != nil {
				return 0, fmt.Errorf("seed ATA accounts: %w", err)
			}
			addresses[index-start] = key
		}
		slot, accounts, err := h.rpc.Accounts(ctx, addresses, rpc.CommitmentConfirmed, 0)
		if err != nil {
			return 0, fmt.Errorf("seed ATA accounts: %w", err)
		}
		if minimum == 0 || slot < minimum {
			minimum = slot
		}
		for index, account := range accounts {
			target := targets[start+index]
			if account == nil {
				empty := sha256.Sum256([]byte("missing:" + target.WalletATA))
				observed := observation{target: target, pubkey: target.WalletATA, amount: 0, mint: target.Mint, slot: slot, source: rpcSeedSource, data: empty[:], received: time.Now().UTC()}
				if _, err := h.persist(ctx, observed); err != nil {
					return 0, err
				}
				continue
			}
			observed, err := decodeObservation(target, target.WalletATA, account.Lamports, account.Owner[:], account.Data, slot, rpcSeedSource, nil, time.Now().UTC())
			if err != nil {
				return 0, err
			}
			if _, err := h.persist(ctx, observed); err != nil {
				return 0, err
			}
		}
	}
	return minimum, nil
}

// decodeObservation settles a closed account, or one that is not a USDC token
// account, to zero on its own bytes: no routeable USDC is at that address.
func decodeObservation(target watch.ATATarget, pubkey string, lamports uint64, ownerBytes, data []byte, slot uint64, source string, signature *string, received time.Time) (observation, error) {
	if slot == 0 || slot > math.MaxInt64 {
		return observation{}, fmt.Errorf("ATA slot is invalid")
	}
	if _, err := publicKey(ownerBytes); err != nil {
		return observation{}, fmt.Errorf("decode ATA owner program: %w", err)
	}
	observed := observation{target: target, pubkey: pubkey, lamports: lamports, mint: target.Mint, slot: slot, source: source, signature: signature, data: data, received: received}
	if lamports == 0 {
		return observed, nil
	}
	held, err := spl.DecodeTokenAccount(&chain.Account{Owner: solana.PublicKeyFromBytes(ownerBytes), Lamports: lamports, Data: data})
	if err != nil || held.Program != solana.TokenProgramID || held.Mint.String() != usdcMint {
		return observed, nil
	}
	if held.Amount > math.MaxInt64 {
		return observation{}, fmt.Errorf("ATA amount exceeds PostgreSQL BIGINT")
	}
	tokenOwner := held.Owner.String()
	observed.amount, observed.owner, observed.mint = held.Amount, &tokenOwner, usdcMint
	return observed, nil
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
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = workersdb.WithTx(writeCtx, h.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Sequence allocation alone does not order commits. Take both stream
		// writer locks before nextval and retain them through publication so a
		// later committed ID cannot pass an earlier uncommitted v2 observation.
		// This also waits for retained inserts; historical retained-only gaps
		// require catch-up before transferring ownership of the scalar cursor.
		lock := fmt.Sprintf("LOCK TABLE %s.balance_sweep_wallet_ata_observation_dedupe, %s.balance_sweep_wallet_ata_observations IN SHARE ROW EXCLUSIVE MODE", h.schema, h.schema)
		if _, err := tx.Exec(writeCtx, lock); err != nil {
			return err
		}
		return tx.QueryRow(writeCtx, query, dedupe, commitment, observed.pubkey, int64(observed.slot), hash, observed.target.Cluster, observed.target.ID, observed.target.Wallet, observed.target.Vault, observed.target.VaultATA, int64(observed.amount), observed.owner, observed.mint, observed.received, observed.source, observed.signature, rawBase64, evidence).Scan(&outcome.EventID, &outcome.Inserted)
	})
	if err != nil {
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
