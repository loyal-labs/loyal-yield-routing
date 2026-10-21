package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sort"
)

const maxFeePayerCandidates = 16

type FeePayerShard struct {
	Payer                                                      string
	MinBalance, MaxBalance, MaxWindowSpend, MaxTxFee, Reserved int64
}

// RankFeePayers orders a vault's candidate payers best first. Every route
// write-locks its fee payer, so one shared payer serializes the whole fleet
// within a block; ranking per vault spreads vaults over the registry.
func RankFeePayers(cluster, vault string, payers []string) []string {
	score := func(payer string) []byte {
		key, err := decodePublicKey(payer)
		if err != nil {
			return nil
		}
		h := sha256.New()
		h.Write([]byte(cluster))
		h.Write([]byte{0})
		h.Write([]byte(vault))
		h.Write([]byte{0})
		h.Write(key[:])
		return h.Sum(nil)
	}
	ranked := []string{}
	for _, payer := range payers {
		if score(payer) != nil {
			ranked = append(ranked, payer)
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if c := bytes.Compare(score(ranked[i]), score(ranked[j])); c != 0 {
			return c > 0
		}
		return ranked[i] < ranked[j]
	})
	if len(ranked) > maxFeePayerCandidates {
		ranked = ranked[:maxFeePayerCandidates]
	}
	return ranked
}

func (s FeePayerShard) healthy(balance, fee int64) bool {
	return fee <= s.MaxTxFee && s.Reserved+fee <= s.MaxWindowSpend &&
		balance >= s.MinBalance && balance-fee >= s.MinBalance && balance <= s.MaxBalance
}

func (s *Store) EligibleFeePayerShards(ctx context.Context, cluster, policySigner string, mounted []string) ([]FeePayerShard, error) {
	rows, err := s.pool.Query(ctx, `SELECT fee_payer,minimum_balance_lamports,maximum_balance_lamports,maximum_window_spend_lamports,maximum_transaction_fee_lamports,current_window_reserved_lamports
FROM loyal_yield.route_fee_payer_shard_status
WHERE cluster=$1 AND enabled AND database_authority_separation_passes AND fee_payer=ANY($2) AND fee_payer<>$3 ORDER BY fee_payer`, cluster, mounted, policySigner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shards := []FeePayerShard{}
	for rows.Next() {
		var shard FeePayerShard
		if err := rows.Scan(&shard.Payer, &shard.MinBalance, &shard.MaxBalance, &shard.MaxWindowSpend, &shard.MaxTxFee, &shard.Reserved); err != nil {
			return nil, err
		}
		shards = append(shards, shard)
	}
	return shards, rows.Err()
}
