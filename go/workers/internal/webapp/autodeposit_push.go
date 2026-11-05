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
}

const autodepositPushSendTimeout = 5 * time.Second

func (p *WalletPush) send(ctx context.Context, walletAddress string, payload autodepositWalletPush) {
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, autodepositPushSendTimeout)
	defer cancel()
	if _, err := p.sendToWallet(ctx, walletAddress, payload.Title, payload.Body, nil); err != nil {
		slog.WarnContext(ctx, "[wallet-push] push send failed", "errorMessage", err.Error(), "walletAddress", walletAddress)
	}
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
