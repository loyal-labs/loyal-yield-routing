package earn

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/solanarpc"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/observer/watch"
	sp "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2"
)

// Behavior ported from earn_reconciliation.rs tests.

func parseTransaction(t *testing.T, value string) jsonTransaction {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader([]byte(value)))
	decoder.UseNumber()
	var out jsonTransaction
	if err := decoder.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func testVault(role, account string) watch.Vault {
	return watch.Vault{Environment: "mainnet-beta", Settings: "settings", Wallet: "wallet-owner", Vault: "vault-owner", VaultIndex: 1,
		Accounts: []watch.Account{{Pubkey: account, Role: role}}}
}

func testUpdate(kind, account string) NormalizedUpdate {
	signature := "signature"
	return NormalizedUpdate{Filters: []string{"earn"}, EventKind: kind, AccountPubkey: &account, Slot: 10, Signature: &signature}
}

func cashFlowTransaction(pre, post string) string {
	usdc := usdcMint.String()
	return `{"slot":1,"meta":{"preTokenBalances":[{"accountIndex":0,"mint":"` + usdc + `","owner":"wallet-owner","uiTokenAmount":{"amount":"` + pre + `"}}],` +
		`"postTokenBalances":[{"accountIndex":0,"mint":"` + usdc + `","owner":"wallet-owner","uiTokenAmount":{"amount":"` + post + `"}}],` +
		`"preBalances":[1000000],"postBalances":[995000],"fee":5000},"transaction":{"message":{"accountKeys":["wallet-owner","vault-owner"]}}}`
}

func TestWalletCashFlowClassification(t *testing.T) {
	vault := testVault("wallet_token", "wallet-ata")
	for _, tc := range []struct {
		name, pre, post string
		kind            cashFlowKind
		amount          uint64
	}{{"deposit", "125", "25", cashDeposit, 100}, {"partial withdrawal", "10", "35", cashWithdrawal, 25}} {
		flow, err := classifyCashFlow(parseTransaction(t, cashFlowTransaction(tc.pre, tc.post)), testUpdate("account_updated", "wallet-ata"), vault)
		if err != nil || flow == nil || flow.kind != tc.kind || flow.amount != tc.amount || flow.mint != usdcMint.String() {
			t.Fatalf("%s = %+v, %v", tc.name, flow, err)
		}
	}
	unanchored := testVault("wallet_token", "wallet-ata")
	unanchored.Vault = "other-vault"
	if flow, err := classifyCashFlow(parseTransaction(t, cashFlowTransaction("125", "25")), testUpdate("account_updated", "wallet-ata"), unanchored); err != nil || flow != nil {
		t.Fatalf("a transaction that never touched the vault is not Earn cash flow: %+v %v", flow, err)
	}
}

func TestPolicyRefundIsCreditedNetOfFee(t *testing.T) {
	vault := testVault("policy", "policy-account")
	transaction := parseTransaction(t, `{"slot":30,"meta":{"preTokenBalances":[],"postTokenBalances":[],"preBalances":[1000000],"postBalances":[1995000],"fee":5000},"transaction":{"message":{"accountKeys":["wallet-owner"]}}}`)
	flow, err := classifyCashFlow(transaction, testUpdate("account_deleted", "policy-account"), vault)
	if err != nil || flow == nil || flow.kind != cashRefund || flow.refundKind != "policy" || flow.amount != 1_000_000 {
		t.Fatalf("policy refund = %+v, %v", flow, err)
	}
}

func TestIdleSweepWithoutKaminoWithdrawIsNotAReserveWithdrawal(t *testing.T) {
	sweep := parseTransaction(t, `{"transaction":{"message":{"instructions":[{"programId":"SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG","data":"1111"}]}},
		"meta":{"innerInstructions":[{"index":0,"instructions":[{"programId":"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA","data":"1111"}]}]}}`)
	if len(sweep.kaminoWithdrawInstructions()) != 0 {
		t.Fatal("an idle vault sweep was treated as a Kamino withdrawal")
	}
	withdrawal := parseTransaction(t, `{"transaction":{"message":{"instructions":[]}},"meta":{"innerInstructions":[{"index":0,"instructions":[
		{"programId":"`+klendProgram.String()+`","data":"`+base58.Encode(withdrawV2Discriminator)+`"}]}]}}`)
	if len(withdrawal.kaminoWithdrawInstructions()) != 1 {
		t.Fatal("a Kamino withdraw v2 was not recognized")
	}
}

func TestHistoricalAccountCloseRequiresExplicitPreAndPostEvidence(t *testing.T) {
	transaction := parseTransaction(t, `{"transaction":{"message":{"accountKeys":[{"pubkey":"payer"},{"pubkey":"obligation"}]}},"meta":{"preBalances":[10,20],"postBalances":[10,0]}}`)
	if !transaction.closedAccount("obligation") || transaction.closedAccount("payer") || transaction.closedAccount("unwatched") {
		t.Fatal("close evidence misclassified")
	}
	missing := parseTransaction(t, `{"transaction":{"message":{"accountKeys":[{"pubkey":"payer"},{"pubkey":"obligation"}]}},"meta":{"preBalances":[10,20],"postBalances":[10]}}`)
	alreadyEmpty := parseTransaction(t, `{"transaction":{"message":{"accountKeys":[{"pubkey":"payer"},{"pubkey":"obligation"}]}},"meta":{"preBalances":[10,0],"postBalances":[10,0]}}`)
	if missing.closedAccount("obligation") || alreadyEmpty.closedAccount("obligation") {
		t.Fatal("unknown balances were read as a close")
	}
}

// Regression: Go dropped Rust's account_deleted exclusion, so a policy-close
// deletion arriving with a wallet update in one transaction shared the
// policy-discovery key and its cleanup proof job was never persisted.
func TestPolicyCloseKeepsADurableJobAfterWalletDiscovery(t *testing.T) {
	vault := testVault("policy", "policy-account")
	wallet := testUpdate("account", vault.Wallet)
	wallet.Filters = []string{watch.EarnWallets}
	deletion := testUpdate("account_deleted", "policy-account")
	deletion.Filters = []string{watch.EarnPolicyAccounts}
	walletKey, deletionKey := durableEventKey(wallet, []watch.Vault{vault}), durableEventKey(deletion, []watch.Vault{vault})
	if walletKey == deletionKey {
		t.Fatalf("policy deletion shares the discovery job key %s", walletKey)
	}
	deletion.EventKey = &deletionKey
	if durableEventKey(deletion, []watch.Vault{vault}) != deletionKey || !isPolicyDeletion(deletion, vault) || isPolicyDeletion(wallet, vault) {
		t.Fatal("policy deletion identity is not stable")
	}
}

func TestOnlyTerminalFailuresDeadLetter(t *testing.T) {
	if deferralKind(errProofPending) != deferProofPending {
		t.Fatal("a pending proof is not a failure")
	}
	if deferralKind(&solanarpc.RPCError{Method: "getMultipleAccounts", Code: solanarpc.MinContextSlotNotReached}) != deferRPCBehind {
		t.Fatal("an RPC behind its minimum context slot is not a failure")
	}
	if deferralKind(errors.New("route policy missing")) != deferFailure {
		t.Fatal("a terminal error was not classified as a failure")
	}
}

func TestJSONPolicyTransactionDecodesSquadsInstructions(t *testing.T) {
	raw, err := os.ReadFile("../../../testdata/earn/squads-policy-create-json.json")
	if err != nil {
		t.Fatal(err)
	}
	signature := "5SyQHcNK5xFLgKFQUmibNDrDNarXz7FCoJpjgvwjmb2ogPDUiWxQyWXKnziBdp92Rbc69JmMNY9YZsPeWbs7MyG9"
	transaction, err := decodeRPCPolicyTransaction(raw, signature, 448_495_297)
	if err != nil || transaction == nil {
		t.Fatal(err)
	}
	settings, wallet := solana.MustPublicKeyFromBase58("4PQiGQn4AkkxPP4agjkbqwSDtnmB3wsHUcWmoGKEXDZJ"), solana.MustPublicKeyFromBase58("FtCYES2CLXxKpVGxqEATmkBYg7RDMGz4zjMiBkFUszNU")
	if len(transaction.Signers) != 1 || transaction.Signers[0] != wallet {
		t.Fatalf("signers = %v", transaction.Signers)
	}
	var squads []sp.Instruction
	for _, instruction := range transaction.Instructions {
		if instruction.ProgramID == sp.Program {
			squads = append(squads, instruction)
		}
	}
	if len(squads) == 0 {
		t.Fatal("outer Squads instruction was dropped")
	}
	foundSettings, foundWallet := false, false
	for _, account := range squads[0].Accounts {
		foundSettings = foundSettings || account.PublicKey == settings && account.IsWritable && !account.IsSigner
		foundWallet = foundWallet || account.PublicKey == wallet && account.IsSigner
	}
	if !foundSettings || !foundWallet {
		t.Fatal("settings must be writable and the wallet must sign")
	}
	if _, err := decodeRPCPolicyTransaction(raw, signature, 448_495_298); err == nil {
		t.Fatal("slot drift was accepted")
	}
}

func TestStreamPolicyTransactionMemoLocations(t *testing.T) {
	keys := [][]byte{make([]byte, 32), memoProgram.Bytes(), sp.Program.Bytes()}
	keys[0][0] = 1
	info := &pb.SubscribeUpdateTransactionInfo{
		Signature: make([]byte, 64),
		Transaction: &pb.Transaction{Message: &pb.Message{
			Header:      &pb.MessageHeader{NumRequiredSignatures: 1, NumReadonlyUnsignedAccounts: 2},
			AccountKeys: keys,
			Instructions: []*pb.CompiledInstruction{
				{ProgramIdIndex: 2, Accounts: []byte{0}, Data: []byte{9}},
				{ProgramIdIndex: 1, Accounts: []byte{0}, Data: []byte("outer")},
			},
		}},
		Meta: &pb.TransactionStatusMeta{InnerInstructions: []*pb.InnerInstructions{{Index: 2, Instructions: []*pb.InnerInstruction{
			{ProgramIdIndex: 1, Accounts: []byte{0}, Data: []byte("inner")},
		}}}},
	}
	transaction, err := DecodeStreamPolicyTransaction(info, 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(transaction.Instructions) != 1 || !transaction.Instructions[0].Accounts[0].IsSigner || !transaction.Instructions[0].Accounts[0].IsWritable {
		t.Fatalf("Squads instruction metas = %+v", transaction.Instructions)
	}
	if len(transaction.Memos) != 2 || transaction.Memos[0].SourceIndex != 256 || transaction.Memos[1].SourceIndex != 513 {
		t.Fatalf("memo locations = %+v", transaction.Memos)
	}
	info.Meta.Err = &pb.TransactionError{Err: []byte{1}}
	if failed, err := DecodeStreamPolicyTransaction(info, 9); err != nil || failed != nil {
		t.Fatal("a failed transaction changes no projected state")
	}
}

func TestEarnMaxIntentMemoGrammar(t *testing.T) {
	destination := solana.SystemProgramID.String()
	amount := uint64(42)
	for _, tc := range []struct {
		memo   string
		want   *EarnMaxIntent
		failed bool
	}{
		{"loyal:earn-max:v2:withdraw:request-1:42:" + destination, &EarnMaxIntent{Withdraw: &EarnMaxWithdrawIntent{RequestID: "request-1", DestinationAccount: destination, AmountRaw: &amount}}, false},
		{"loyal:earn-max:v2:withdraw:request-1:+42:" + destination, &EarnMaxIntent{Withdraw: &EarnMaxWithdrawIntent{RequestID: "request-1", DestinationAccount: destination, AmountRaw: &amount}}, false},
		{"loyal:earn-max:v2:withdraw:request-1:max:" + destination, &EarnMaxIntent{Withdraw: &EarnMaxWithdrawIntent{RequestID: "request-1", DestinationAccount: destination}}, false},
		{"loyal:earn-max:v2:cancel:request-1", &EarnMaxIntent{Cancel: &EarnMaxCancelIntent{RequestID: "request-1"}}, false},
		{"loyal:earn-max:v2:deposit:anything", nil, false},
		{"loyal:earn-max:v2:claim", nil, false},
		{"hello", nil, false},
		{"loyal:earn-max:v2:withdraw:request-1:0:" + destination, nil, true},
		{"loyal:earn-max:v2:withdraw:short:1:" + destination, nil, true},
		{"loyal:earn-max:v2:withdraw:request-1:1:not-a-key", nil, true},
		{"loyal:earn-max:v2:unknown", nil, true},
	} {
		got, err := parseEarnMaxIntent([]byte(tc.memo))
		if tc.failed != (err != nil) {
			t.Fatalf("%s error = %v", tc.memo, err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(tc.want)
		if !bytes.Equal(gotJSON, wantJSON) {
			t.Fatalf("%s = %s, want %s", tc.memo, gotJSON, wantJSON)
		}
	}
	if intent, err := parseEarnMaxIntent([]byte{0xff, 0xfe}); intent != nil || err != nil {
		t.Fatal("non-UTF-8 memo data is not an intent")
	}
}
