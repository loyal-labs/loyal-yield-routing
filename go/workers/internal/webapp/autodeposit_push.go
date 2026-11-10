package webapp

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

type autodepositWalletPush struct {
	Body  string `json:"body"`
	Title string `json:"title"`

	// Zero values are deliberate: an unset push is an immediate account alert.
	Category        pushCategory
	Delivery        pushDelivery
	Route           string
	IdempotencyName string
}

const autodepositPushSendTimeout = 10 * time.Second

func (p *WalletPush) send(ctx context.Context, walletAddress string, payload autodepositWalletPush) bool {
	if p == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, autodepositPushSendTimeout)
	defer cancel()
	sent, err := p.sendToWallet(ctx, walletAddress, payload, nil)
	if err != nil {
		slog.WarnContext(ctx, "[wallet-push] push send failed", "errorMessage", err.Error(), "walletAddress", walletAddress)
		return false
	}
	if !sent {
		slog.InfoContext(ctx, "[wallet-push] wallet has no subscribed device", "walletAddress", walletAddress)
	}
	return sent
}

func autodepositEnabledPush() autodepositWalletPush {
	return autodepositWalletPush{
		Body:  "Your idle USDC will be moved to Earn automatically, roughly once an hour.",
		Title: "Auto-deposit is on.",
	}
}

func autodepositSweepScheduledPush(amountRaw uint64) autodepositWalletPush {
	amount := "USDC"
	if amountRaw > 0 {
		amount = formatAutodepositUSDC(amountRaw)
	}
	return autodepositWalletPush{
		Body:  "Heading to yield in ~1 hour. Adjust autodeposit limits if you need it liquid.",
		Title: fmt.Sprintf("%s about to move to EARN.", amount),
	}
}
