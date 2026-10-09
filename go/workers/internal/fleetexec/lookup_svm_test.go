package fleetexec

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/programs/system"
)

type lookupSVM struct {
	mu     sync.Mutex
	input  io.WriteCloser
	output *bufio.Reader
	rpc    *LookupRPC
}

func startLookupSVM(t *testing.T, f lookupFixture) *lookupSVM {
	t.Helper()
	path := os.Getenv("LOOKUP_TEST_SVM_PATH")
	if path == "" {
		t.Skip("requires compiled local ALT-program bank helper")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	command := exec.CommandContext(ctx, path)
	command.Env = []string{"LC_ALL=C"}
	for _, name := range []string{"SQUADS_SMART_ACCOUNT_PROGRAM_SO", "SUBSCRIPTIONS_PROGRAM_SO", "MOCK_YIELD_PROTOCOLS_PROGRAM_SO"} {
		if v := os.Getenv(name); v != "" {
			command.Env = append(command.Env, name+"="+v)
		}
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	input, err := command.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	svm := &lookupSVM{input: input, output: bufio.NewReader(output)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid fixture request", 400)
			return
		}
		svm.mu.Lock()
		defer svm.mu.Unlock()
		if _, err = svm.input.Write(append(b, '\n')); err != nil {
			http.Error(w, "fixture stopped", 500)
			return
		}
		b, err = svm.output.ReadBytes('\n')
		if err != nil {
			http.Error(w, "fixture stopped", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(func() {
		cancel()
		_ = input.Close()
		server.Close()
		select {
		case err := <-done:
			if ctx.Err() == nil && err != nil {
				t.Errorf("local bank: %v %s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Error("local bank did not join")
		}
	})
	svm.rpc, err = NewLookupRPC(server.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = svm.direct("initialize", map[string]any{"accounts": f.Accounts}, nil); err != nil {
		t.Fatal(err)
	}
	return svm
}
func (s *lookupSVM) direct(method string, params any, out any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if _, err := s.input.Write(append(request, '\n')); err != nil {
		return err
	}
	b, err := s.output.ReadBytes('\n')
	if err != nil {
		return err
	}
	var reply struct {
		Result json.RawMessage
		Error  *struct{ Message string }
	}
	if err = json.Unmarshal(b, &reply); err != nil {
		return err
	}
	if reply.Error != nil {
		return fmt.Errorf("local bank: %s", reply.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(reply.Result, out)
	}
	return nil
}
func TestLookupGoPacketsExecuteActualALTProgram(t *testing.T) {
	f := readLookupFixture(t)
	svm := startLookupSVM(t, f)
	ctx := t.Context()
	intent := lookupFixtureIntent(f)
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	execute := func(i LookupIntent, slot int64) (LookupAttempt, *LookupReceipt) {
		t.Helper()
		hash, height, contextSlot, err := svm.rpc.LookupBlockhash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := signLookupMutation(i, hash, height, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = svm.rpc.SimulateLookup(ctx, wire.SignedTransaction); err != nil {
			t.Fatal(err)
		}
		if err = svm.rpc.SendWire(ctx, wire.SignedTransaction, true); err != nil {
			t.Fatal(err)
		}
		receipt, err := svm.rpc.LookupFinalizedReceipt(ctx, wire.TransactionSignature)
		if err != nil || receipt == nil || receipt.Slot != slot {
			t.Fatalf("actual receipt %+v %v", receipt, err)
		}
		return LookupAttempt{Intent: i, Wire: wire, SigningContextSlot: contextSlot}, receipt
	}
	recover := func(a LookupAttempt, receipt *LookupReceipt, warmed bool) lookupRecovery {
		t.Helper()
		status, err := svm.rpc.SignatureStatus(ctx, a.Wire.TransactionSignature)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, receipt.Slot)
		if err != nil {
			t.Fatal(err)
		}
		result, err := recoverLookup(a, status, receipt, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if warmed && result.proof == nil {
			t.Fatalf("actual proof held: %s", result.wait)
		}
		if !warmed && result.proof != nil {
			t.Fatal("new membership activated in same bank")
		}
		return result
	}
	created, receipt := execute(intent, 1000)
	recover(created, receipt, false)
	if err := svm.direct("advanceSlot", []any{1001}, nil); err != nil {
		t.Fatal(err)
	}
	recover(created, receipt, true)
	// A v0 System transfer loads a writable destination and a separate readonly
	// address from the actual warmed table. The real bank validates ALT loading.
	hash, _, _, err := svm.rpc.LookupBlockhash(ctx)
	if err != nil {
		t.Fatal(err)
	}
	manager := sdk.MustPublicKeyFromBase58(f.Manager)
	transfer := system.NewTransferInstruction(1, manager, sdk.MustPublicKeyFromBase58(f.Addresses[0])).Build()
	data, _ := transfer.Data()
	metas := append(transfer.Accounts(), sdk.Meta(sdk.MustPublicKeyFromBase58(f.Addresses[1])))
	tx, err := sdk.NewTransaction([]sdk.Instruction{sdk.NewInstruction(sdk.SystemProgramID, metas, data)}, sdk.MustHashFromBase58(hash), sdk.TransactionPayer(manager), sdk.TransactionAddressTables(map[sdk.PublicKey]sdk.PublicKeySlice{sdk.MustPublicKeyFromBase58(f.Table): {sdk.MustPublicKeyFromBase58(f.Addresses[0]), sdk.MustPublicKeyFromBase58(f.Addresses[1])}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Message.AddressTableLookups) != 1 || len(tx.Message.AddressTableLookups[0].ReadonlyIndexes) != 1 {
		t.Fatal("mature test did not exercise actual readonly ALT lookup")
	}
	private := sdk.PrivateKey(key)
	if _, err = tx.Sign(func(k sdk.PublicKey) *sdk.PrivateKey {
		if k == manager {
			return &private
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	packet, err := tx.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err = svm.rpc.SendWire(ctx, packet, true); err != nil {
		t.Fatalf("actual mature readonly bank lookup: %v", err)
	}
	extendedIntent := intent
	extendedIntent.Kind = LookupExtend
	extendedIntent.RecentSlot = nil
	extendedIntent.Prefix = f.Addresses[:2]
	extendedIntent.Extension = f.Addresses[2:]
	extended, receipt := execute(extendedIntent, 1001)
	recover(extended, receipt, false)
	if err = svm.direct("advanceSlot", []any{1002}, nil); err != nil {
		t.Fatal(err)
	}
	recover(extended, receipt, true)
	deactivatedIntent := extendedIntent
	deactivatedIntent.Kind = LookupDeactivate
	deactivatedIntent.Prefix = f.Addresses
	deactivatedIntent.Extension = nil
	deactivated, receipt := execute(deactivatedIntent, 1002)
	recover(deactivated, receipt, true)
	closeIntent := deactivatedIntent
	closeIntent.Kind = LookupClose
	closeIntent.Recipient = f.Manager
	deactivation := uint64(1002)
	closeIntent.ExpectedDeactivationSlot = &deactivation
	blocked := func() {
		t.Helper()
		snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, 1002)
		if err != nil {
			t.Fatal(err)
		}
		if lookupCloseReady(snapshot, deactivation) {
			t.Fatal("close bypassed actual produced-slot cooldown")
		}
		hash, height, _, err := svm.rpc.LookupBlockhash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wire, err := signLookupMutation(closeIntent, hash, height, key)
		if err != nil {
			t.Fatal(err)
		}
		if err = svm.rpc.SimulateLookup(ctx, wire.SignedTransaction); err == nil {
			t.Fatal("actual ALT program accepted close before cooldown")
		}
	}
	blocked()
	if err = svm.direct("advanceSlot", []any{2000}, nil); err != nil {
		t.Fatal(err)
	}
	blocked()
	for slot := int64(2001); slot <= 2513; slot++ {
		if err = svm.direct("advanceSlot", []any{slot}, nil); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := svm.rpc.LookupSnapshot(ctx, f.Table, 2513)
	if err != nil || !lookupCloseReady(snapshot, deactivation) {
		t.Fatalf("actual produced-slot cooldown never elapsed: %+v %v", snapshot, err)
	}
	closed, receipt := execute(closeIntent, 2513)
	recover(closed, receipt, true)
	if err = svm.rpc.SendWire(ctx, closed.Wire.SignedTransaction, true); err != nil {
		t.Fatal(err)
	}
	again, err := svm.rpc.LookupFinalizedReceipt(ctx, closed.Wire.TransactionSignature)
	if err != nil || !bytes.Equal(again.Wire, receipt.Wire) {
		t.Fatal("exact close retry lost packet identity")
	}
	t.Log("Go create/extend -> actual ALT program; finalized warmed readonly v0 lookup; deactivate; actual retained SlotHashes hold and 513 produced fixture-bank advances; exact close rent refund; no consensus/PoH or mainnet fee proof")
}
