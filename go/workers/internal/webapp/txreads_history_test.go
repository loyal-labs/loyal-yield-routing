package webapp

import (
	"testing"
	"time"
)

func TestRebalanceSourceSurvivesDestinationSnapshot(t *testing.T) {
	mainMarket := "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"
	onreMarket := "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"
	const mint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	seed := txreadsPositionSeed{initialReserve: "main-reserve", initialMarket: &mainMarket, initialLiquidityMint: mint}
	decisionID := int64(1)
	events := []txreadsHoldingEvent{
		{id: 1, eventType: "snapshot_reconciled", observedSlot: 10, observedAt: time.Unix(10, 0), reserve: "onre-reserve", market: &onreMarket, liquidityMint: mint},
		{id: 2, eventType: "rebalance_confirmed", observedSlot: 11, observedAt: time.Unix(11, 0), reserve: "onre-reserve", market: &onreMarket, liquidityMint: mint, sourceRebalanceDecision: &decisionID},
	}
	history := replayPositionHistory(seed, events, nil, map[int64]txreadsRebalance{
		decisionID: {amountRaw: 199414480, sourceReserve: &seed.initialReserve},
	}, nil)
	transactions := txreadsAssembleTransactions(history, nil)
	if len(transactions) != 1 {
		t.Fatalf("expected one rebalance, got %d", len(transactions))
	}
	tx := transactions[0]
	if tx.Source.Label != "Main Kamino" || history[1].sourceMarket == nil || *history[1].sourceMarket != mainMarket {
		t.Fatalf("snapshot overwrote the rebalance source: %+v", tx)
	}
	if tx.Destination.Label != "OnRe" || tx.RawAmount != "$199.414480" {
		t.Fatalf("rebalance destination or amount changed: %+v", tx)
	}
}
