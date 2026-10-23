package earnprep

import (
	"errors"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// The policy-close and token-account-close pieces the Earn cleanup flow
// builds, mirrors of the TS builders in web/packages/smart-account-vaults/src
// (CL = client.ts):

// WithdrawPolicyRemoveSync ports prepareCloseYieldRoutingPoliciesSync's
// instruction (CL:11791-11866 with executeSettingsTransactionSync.ts): one
// execute_settings_transaction_sync whose action list is a PolicyRemove per
// policy. Metas are settings w, rentPayer w+s, SystemProgram, the Squads
// program, the member signer s read-only, then every policy writable; data is
// the discriminator, one signer, the action count, a [9] ++ policy action per
// policy, and the empty memo. The TS builder emits exactly that; the Go
// reuses squads.PolicyApply, whose Instruction with creates=0 produces the
// same bytes. Cleanup always signs with the wallet, so the helper takes one
// signer like the TS call site's deduped single-element list.
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
