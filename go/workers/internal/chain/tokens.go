package chain

import (
	"context"
	"errors"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// TokenAccounts lists owner's accounts of one token program at commitment,
// read no older than minContextSlot.
func (c *Client) TokenAccounts(ctx context.Context, owner, program solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (slot uint64, accounts []Account, err error) {
	opts := &rpc.GetTokenAccountsOpts{Commitment: commitment, Encoding: solana.EncodingBase64}
	if minContextSlot > 0 {
		opts.MinContextSlot = &minContextSlot
	}
	out, err := c.rpc.GetTokenAccountsByOwner(ctx, owner, &rpc.GetTokenAccountsConfig{ProgramId: &program}, opts)
	if err != nil {
		return 0, nil, failed("getTokenAccountsByOwner", err)
	}
	if out == nil || out.Context.Slot == 0 || out.Context.Slot < minContextSlot {
		return 0, nil, errors.New("getTokenAccountsByOwner: response does not match the request")
	}
	accounts = make([]Account, 0, len(out.Value))
	for _, value := range out.Value {
		if value == nil || value.Account.Data == nil {
			return 0, nil, errors.New("getTokenAccountsByOwner: incomplete account")
		}
		accounts = append(accounts, Account{Key: value.Pubkey, Owner: value.Account.Owner, Lamports: value.Account.Lamports, Data: value.Account.Data.GetBinary(), Executable: value.Account.Executable})
	}
	return out.Context.Slot, accounts, nil
}
