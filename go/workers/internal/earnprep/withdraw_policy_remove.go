package earnprep

import (
	"errors"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

func WithdrawPolicyRemoveSync(settings, rentPayer, signer solana.PublicKey, policies []solana.PublicKey) (squads.Instruction, error) {
	if len(policies) == 0 {
		return squads.Instruction{}, errors.New("earnprep: policy remove needs at least one policy")
	}
	return squads.PolicyApply{
		Settings:  settings,
		RentPayer: rentPayer,
		Signer:    signer,
		Replace:   policies,
	}.Instruction()
}

// WithdrawCloseAccount ports makeSignerWritable(
// createCloseAccountInstruction(account, destination, owner, [], tokenProgram))
// (CL:2984-2996, spl-token close-account): data [9], metas [account w,
// destination w, authority s], with the authority meta flipped to writable
// because the vault authority signs the inner instruction by CPI and every
// vault-signed meta must carry signer and writable.
func WithdrawCloseAccount(tokenProgram, account, destination, authority solana.PublicKey) squads.Instruction {
	return squads.Instruction{
		ProgramID: tokenProgram,
		Accounts: []solana.AccountMeta{
			{PublicKey: account, IsWritable: true},
			{PublicKey: destination, IsWritable: true},
			{PublicKey: authority, IsSigner: true, IsWritable: true},
		},
		Data: []byte{9},
	}
}
