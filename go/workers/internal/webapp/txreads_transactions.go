package webapp

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Turns the history records into the wire shape and applies the formatter's
// collapse and the route's ordering (earn-transactions/formatter.ts and
// route.ts sortEarnTransactions).

func txreadsSerializePositionEvent(event txreadsPositionEvent) SerializedEarnTransaction {
	kind := "deposit"
	switch event.typ {
	case "withdrawal":
		kind = "withdraw"
	case "rebalance":
		kind = "rebalance"
	case "reconciliation":
		kind = "reconciliation"
	}
	direction := "neutral"
	switch kind {
	case "deposit":
		direction = "in"
	case "withdraw":
		direction = "out"
	}
	amount := event.amountRaw
	switch kind {
	case "withdraw":
		switch {
		case event.withdrawnAmountRaw != nil:
			amount = *event.withdrawnAmountRaw
		case event.principalDeltaRaw != nil:
			amount = *event.principalDeltaRaw
		}
	case "deposit":
		if event.principalDeltaRaw != nil {
			amount = *event.principalDeltaRaw
		}
	case "rebalance":
		if event.rebalanceAmountRaw != nil {
			amount = *event.rebalanceAmountRaw
		}
	default:
		if event.principalAmountRaw > 0 {
			amount = event.principalAmountRaw
		}
	}
	isMovement := kind == "rebalance" || kind == "reconciliation"
	sourceLabel := txreadsEarnVaultLabel
	destinationLabel := txreadsMainUSDCLabel
	if kind == "deposit" {
		sourceLabel = txreadsMainUSDCLabel
		destinationLabel = txreadsEarnVaultLabel
	}
	if isMovement {
		sourceLabel = txreadsMarketLabel(event.sourceLiquidityMint, event.sourceMarket, event.sourceReserve)
		destinationLabel = txreadsMarketLabel(event.destinationLiquidityMint, event.destinationMarket, event.destinationReserve)
	}
	positionIcon := txreadsMarketIcon(event.market)
	sourceIcon := positionIcon
	if kind == "deposit" {
		sourceIcon = txreadsMainUSDCIcon
	}
	if isMovement {
		sourceIcon = txreadsMarketIcon(event.sourceMarket)
	}
	destinationIcon := txreadsMainUSDCIcon
	if kind == "deposit" {
		destinationIcon = positionIcon
	}
	if isMovement {
		destinationIcon = txreadsMarketIcon(event.destinationMarket)
	}
	confirmedAt := isoMilliseconds(event.confirmedAt)
	liquidityMint := event.liquidityMint
	return SerializedEarnTransaction{
		Amount:        txreadsFormatDisplayUSDC(amount, direction),
		ConfirmedAt:   confirmedAt,
		ConfirmedSlot: strconv.FormatInt(event.confirmedSlot, 10),
		DateGroup:     txreadsDateGroup(event.confirmedAt),
		Destination:   EarnTransactionAccount{Icon: txreadsIconPtr(destinationIcon), Label: destinationLabel},
		EventType:     event.eventType,
		ID:            event.signature + ":" + strconv.FormatInt(event.id, 10),
		Kind:          kind,
		LiquidityMint: &liquidityMint,
		RawAmount:     txreadsFormatExactUSDC(amount),
		Signature:     event.signature,
		SortTimestamp: confirmedAt,
		Source:        EarnTransactionAccount{Icon: txreadsIconPtr(sourceIcon), Label: sourceLabel},
		Timestamp:     txreadsTimestamp(event.confirmedAt),
	}
}

// txreadsMsKey is the millisecond key every Date.getTime comparison uses.
func txreadsMsKey(at time.Time) time.Time {
	return at.UTC().Truncate(time.Millisecond)
}

func txreadsIconPtr(icon string) *string {
	if icon == "" {
		return nil
	}
	return &icon
}

func txreadsSerializeAutodepositEvent(event txreadsAutodepositEvent) SerializedEarnTransaction {
	isBalanceSweep := event.actionType == "balance_sweep"
	isCreate := event.actionType == "create"
	amount := int64(0)
	if isBalanceSweep {
		amount = event.amountRaw
	}
	direction := "neutral"
	eventType := "autodeposit_closed"
	kind := "autodeposit_action"
	if isBalanceSweep {
		direction = "in"
		eventType = "balance_sweep"
		kind = "balance_sweep"
	} else if isCreate {
		eventType = "autodeposit_created"
	}
	sourceIcon := (*string)(nil)
	sourceLabel := txreadsEarnVaultLabel
	if isBalanceSweep || isCreate {
		sourceIcon = txreadsIconPtr(txreadsMainUSDCIcon)
		sourceLabel = txreadsMainUSDCLabel
	}
	destinationLabel := txreadsAutodepositLabel
	if isCreate || isBalanceSweep {
		destinationLabel = txreadsEarnVaultLabel
	}
	confirmedAt := isoMilliseconds(event.confirmedAt)
	return SerializedEarnTransaction{
		Amount:        txreadsFormatDisplayUSDC(amount, direction),
		ConfirmedAt:   confirmedAt,
		ConfirmedSlot: strconv.FormatInt(event.confirmedSlot, 10),
		DateGroup:     txreadsDateGroup(event.confirmedAt),
		Destination:   EarnTransactionAccount{Icon: nil, Label: destinationLabel},
		EventType:     eventType,
		ID:            event.id,
		Kind:          kind,
		LiquidityMint: &event.liquidityMint,
		RawAmount:     txreadsFormatExactUSDC(amount),
		Signature:     event.signature,
		SortTimestamp: confirmedAt,
		Source:        EarnTransactionAccount{Icon: sourceIcon, Label: sourceLabel},
		Timestamp:     txreadsTimestamp(event.confirmedAt),
	}
}

func txreadsSelfMovement(transaction SerializedEarnTransaction) bool {
	return transaction.Source.Label == transaction.Destination.Label
}

// collapseDuplicateEarnRebalanceTransactions: the first transaction wins per
// signature; a later non-self-movement replaces an earlier self-movement in
// place.
func collapseDuplicateRebalanceTransactions(transactions []SerializedEarnTransaction) []SerializedEarnTransaction {
	indexBySignature := map[string]int{}
	collapsed := make([]SerializedEarnTransaction, 0, len(transactions))
	for _, transaction := range transactions {
		if transaction.Kind != "rebalance" || len(transaction.Signature) == 0 {
			collapsed = append(collapsed, transaction)
			continue
		}
		existing, seen := indexBySignature[transaction.Signature]
		if !seen {
			indexBySignature[transaction.Signature] = len(collapsed)
			collapsed = append(collapsed, transaction)
			continue
		}
		if txreadsSelfMovement(collapsed[existing]) && !txreadsSelfMovement(transaction) {
			collapsed[existing] = transaction
		}
	}
	return collapsed
}

// sortEarnTransactions: newest first by confirmation time (millisecond
// precision, as Date.parse sees it), then slot descending, then id ascending.
func sortEarnTransactions(transactions []SerializedEarnTransaction) {
	sort.SliceStable(transactions, func(left, right int) bool {
		a, b := transactions[left], transactions[right]
		if a.SortTimestamp != b.SortTimestamp {
			return a.SortTimestamp > b.SortTimestamp
		}
		aSlot, aErr := strconv.ParseInt(a.ConfirmedSlot, 10, 64)
		bSlot, bErr := strconv.ParseInt(b.ConfirmedSlot, 10, 64)
		if aErr == nil && bErr == nil && aSlot != bSlot {
			return aSlot > bSlot
		}
		return a.ID < b.ID
	})
}

func sortPositionEventsDescending(events []txreadsPositionEvent) {
	sort.SliceStable(events, func(left, right int) bool {
		a, b := events[left], events[right]
		aAt, bAt := txreadsMsKey(a.confirmedAt), txreadsMsKey(b.confirmedAt)
		if !aAt.Equal(bAt) {
			return aAt.After(bAt)
		}
		if a.signature != b.signature {
			return a.signature < b.signature
		}
		return a.id < b.id
	})
}

func sortHoldingEventsChronologically(events []txreadsHoldingEvent) {
	sort.SliceStable(events, func(left, right int) bool {
		a, b := events[left], events[right]
		if a.observedSlot != b.observedSlot {
			return a.observedSlot < b.observedSlot
		}
		aAt, bAt := txreadsMsKey(a.observedAt), txreadsMsKey(b.observedAt)
		if !aAt.Equal(bAt) {
			return aAt.Before(bAt)
		}
		return a.id < b.id
	})
}

func sortAutodepositEventsDescending(events []txreadsAutodepositEvent) {
	sort.SliceStable(events, func(left, right int) bool {
		a, b := events[left], events[right]
		aAt, bAt := txreadsMsKey(a.confirmedAt), txreadsMsKey(b.confirmedAt)
		if !aAt.Equal(bAt) {
			return aAt.After(bAt)
		}
		if a.confirmedSlot != b.confirmedSlot {
			return a.confirmedSlot > b.confirmedSlot
		}
		return a.id < b.id
	})
}

func pgidString(id int64) string {
	return strconv.FormatInt(id, 10)
}

func joinColons(parts []string) string {
	return strings.Join(parts, ":")
}
