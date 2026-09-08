package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"
)

type RPCClient struct {
	url          string
	client       *http.Client
	retryBackoff time.Duration
}

const readOnlyRPCAttempts = 3

type ConfirmedAccount struct {
	Address    string
	Owner      string
	Lamports   uint64
	Data       []byte
	Executable bool
}

type LatestBlockhash struct {
	Blockhash            string
	LastValidBlockHeight int64
}

type MessageFeeObservation struct {
	MessageSHA256 string `json:"messageSha256"`
	Slot          int64  `json:"slot"`
	Lamports      uint64 `json:"lamports"`
}

// ObserveMessageFee asks the chain to price the exact unsigned message,
// including its compute-budget instructions. Null (expired blockhash) or an
// incoherent slot is a HOLD, never a zero-fee assumption.
func (c *RPCClient) ObserveMessageFee(ctx context.Context, message []byte, minimumSlot int64) (MessageFeeObservation, error) {
	if _, err := checkedUnsignedMessage(message); err != nil {
		return MessageFeeObservation{}, err
	}
	if minimumSlot <= 0 {
		return MessageFeeObservation{}, budgetHold("invalid_fee_observation_slot")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value *uint64 `json:"value"`
	}
	if err := c.call(ctx, "getFeeForMessage", []any{base64.StdEncoding.EncodeToString(message), map[string]any{"commitment": "confirmed", "minContextSlot": minimumSlot}}, &result); err != nil {
		return MessageFeeObservation{}, budgetHold("network_fee_unavailable")
	}
	if result.Value == nil || *result.Value == 0 || result.Context.Slot < minimumSlot {
		return MessageFeeObservation{}, budgetHold("network_fee_unavailable")
	}
	return MessageFeeObservation{MessageSHA256: sha256Bytes(message), Slot: result.Context.Slot, Lamports: *result.Value}, nil
}

type TransactionTokenBalance struct {
	Address, OwnerProgram, Mint, Authority string
	Raw                                    uint64
}

type ProgramReturnData struct {
	ProgramID  string
	DataBase64 string
}

type ConfirmedTransactionEvidence struct {
	Finalized         bool
	Signature         string
	Slot              int64
	PreTokenBalances  []TransactionTokenBalance
	PostTokenBalances []TransactionTokenBalance
	ReturnData        *ProgramReturnData
	Logs              []string
}

func NewRPCClient(rpcURL string) (*RPCClient, error) {
	config := RuntimeConfig{DatabaseURL: "validation-only", RPCURL: rpcURL, RouteKey: "validation-only"}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &RPCClient{url: rpcURL, client: &http.Client{Timeout: 15 * time.Second}, retryBackoff: 200 * time.Millisecond}, nil
}

func (c *RPCClient) call(ctx context.Context, method string, params []any, output any) error {
	outputValue := reflect.ValueOf(output)
	if outputValue.Kind() != reflect.Pointer || outputValue.IsNil() {
		return fmt.Errorf("RPC output must be a non-nil pointer")
	}
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	attempts := readOnlyRPCAttempts
	if method == "sendTransaction" {
		attempts = 1
	}
	for attempt := 0; attempt < attempts; attempt++ {
		freshOutput := reflect.New(outputValue.Elem().Type())
		err = c.callOnce(ctx, method, payload, freshOutput.Interface())
		if err == nil {
			outputValue.Elem().Set(freshOutput.Elem())
			return nil
		}
		if attempt+1 == attempts {
			break
		}
		backoff := c.retryBackoff * time.Duration(attempt+1)
		if backoff <= 0 {
			continue
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (c *RPCClient) callOnce(ctx context.Context, method string, payload []byte, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("RPC %s returned HTTP %d", method, response.StatusCode)
	}
	envelope := struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}{}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fmt.Errorf("RPC %s failed", method)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("RPC %s returned no result", method)
	}
	return json.Unmarshal(envelope.Result, output)
}

func (c *RPCClient) ConfirmedSlot(ctx context.Context) (int64, error) {
	var slot int64
	if err := c.call(ctx, "getSlot", []any{map[string]string{"commitment": "confirmed"}}, &slot); err != nil {
		return 0, confirmedObservationUnavailable(err)
	}
	if slot <= 0 {
		return 0, confirmedObservationUnavailable(fmt.Errorf("confirmed slot unavailable"))
	}
	return slot, nil
}

// ConfirmedBlockTime returns the chain's own unix seconds for a slot. Kamino
// oracle freshness is judged against block time, never the worker's clock: the
// two can drift by more than the freshness window the audit requires.
func (c *RPCClient) ConfirmedBlockTime(ctx context.Context, slot int64) (int64, error) {
	var blockTime *int64
	if err := c.call(ctx, "getBlockTime", []any{slot}, &blockTime); err != nil {
		return 0, confirmedObservationUnavailable(err)
	}
	if blockTime == nil || *blockTime <= 0 {
		return 0, confirmedObservationUnavailable(fmt.Errorf("confirmed block time unavailable for slot %d", slot))
	}
	return *blockTime, nil
}

// GetMultipleAccounts reads one coherent confirmed account set. The returned
// context slot is the only slot callers may use for the resulting Snapshot.
func (c *RPCClient) GetMultipleAccounts(
	ctx context.Context,
	addresses []string,
	minContextSlot int64,
) (int64, []ConfirmedAccount, error) {
	return c.getMultipleAccounts(ctx, addresses, minContextSlot, nil)
}

// strategyReceiptFinalized re-reads only the strategy receipt at finalized
// commitment. A null there is settled ledger state; the same null at confirmed
// commitment may still be a replication artifact, so it stays a retryable tick
// error until this read settles the question.
func (c *RPCClient) strategyReceiptFinalized(ctx context.Context, minContextSlot int64) (int64, []ConfirmedAccount, error) {
	return c.getMultipleAccountsAtCommitment(ctx, []string{bridgeStrategyReceipt}, minContextSlot,
		map[string]struct{}{bridgeStrategyReceipt: {}}, "finalized")
}

// GetMultipleAccountsWithOptional permits only caller-pinned lifecycle
// addresses to be absent. Every other account remains mandatory.
func (c *RPCClient) GetMultipleAccountsWithOptional(
	ctx context.Context,
	addresses []string,
	minContextSlot int64,
	optionalAddresses ...string,
) (int64, []ConfirmedAccount, error) {
	optional := make(map[string]struct{}, len(optionalAddresses))
	for _, address := range optionalAddresses {
		found := false
		for _, candidate := range addresses {
			found = found || candidate == address
		}
		if address == "" || !found {
			return 0, nil, fmt.Errorf("optional account must be pinned inside the requested address set")
		}
		optional[address] = struct{}{}
	}
	if len(optional) == 0 {
		return 0, nil, fmt.Errorf("optional account must be pinned inside the requested address set")
	}
	return c.getMultipleAccounts(ctx, addresses, minContextSlot, optional)
}

func (c *RPCClient) getMultipleAccounts(
	ctx context.Context,
	addresses []string,
	minContextSlot int64,
	optionalAddresses map[string]struct{},
) (int64, []ConfirmedAccount, error) {
	return c.getMultipleAccountsAtCommitment(ctx, addresses, minContextSlot, optionalAddresses, "confirmed")
}

// Finalized setup seed reads share the existing decoder; ordinary worker
// observations retain their confirmed default.
func (c *RPCClient) getMultipleAccountsAtCommitment(ctx context.Context, addresses []string, minContextSlot int64, optionalAddresses map[string]struct{}, commitment string) (int64, []ConfirmedAccount, error) {
	if commitment != "confirmed" && commitment != "finalized" {
		return 0, nil, fmt.Errorf("unsupported account commitment")
	}
	if len(addresses) == 0 || minContextSlot <= 0 {
		return 0, nil, fmt.Errorf("account addresses and minContextSlot are required")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []*struct {
			Owner      string          `json:"owner"`
			Lamports   uint64          `json:"lamports"`
			Data       json.RawMessage `json:"data"`
			Executable bool            `json:"executable"`
		} `json:"value"`
	}
	err := c.call(ctx, "getMultipleAccounts", []any{addresses, map[string]any{
		"commitment": commitment, "encoding": "base64", "minContextSlot": minContextSlot,
	}}, &result)
	if err != nil {
		return 0, nil, confirmedObservationUnavailable(err)
	}
	if result.Context.Slot < minContextSlot || len(result.Value) != len(addresses) {
		return 0, nil, confirmedObservationUnavailable(fmt.Errorf("incoherent confirmed account response"))
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for index, value := range result.Value {
		if value == nil {
			if _, optional := optionalAddresses[addresses[index]]; optional {
				accounts[index] = ConfirmedAccount{Address: addresses[index]}
				continue
			}
			return 0, nil, fmt.Errorf("required account %s is absent", addresses[index])
		}
		if value.Owner == "" {
			return 0, nil, confirmedObservationUnavailable(fmt.Errorf("account %s has no owner", addresses[index]))
		}
		var encoded []string
		if err := json.Unmarshal(value.Data, &encoded); err != nil || len(encoded) != 2 || encoded[1] != "base64" {
			return 0, nil, confirmedObservationUnavailable(fmt.Errorf("account %s has invalid encoding", addresses[index]))
		}
		data, err := base64.StdEncoding.DecodeString(encoded[0])
		if err != nil {
			return 0, nil, confirmedObservationUnavailable(fmt.Errorf("decode account %s: %w", addresses[index], err))
		}
		accounts[index] = ConfirmedAccount{
			Address: addresses[index], Owner: value.Owner, Lamports: value.Lamports,
			Data: data, Executable: value.Executable,
		}
	}
	return result.Context.Slot, accounts, nil
}

// ProgramDataDeploySlots resolves each pinned upgradeable program to its
// ProgramData account and returns the last-deploy slot recorded there. The
// read is deliberately two-phase: a program account's 36-byte upgradeable
// header names its ProgramData account, and the u64 at offset 4 of that
// account is the slot of the last deploy. An in-place upgrade therefore
// changes exactly the value this worker compares against its pinned slots.
// programIdentityAccounts is the confirmed-state reader behind M6. It returns
// the pinned program header accounts (36 bytes) and their ProgramData accounts
// (12 bytes, or the full image when full is set). An absent account is a
// confirmed identity failure rather than a transport error, so the watcher can
// hold the route durably while only a failed call surfaces as an error.
func (c *RPCClient) programIdentityAccounts(ctx context.Context, full bool) ([]programIdentityImage, error) {
	programs := []string{bridgeVoltrProgram, bridgeAdaptorProgram}
	programDataAddresses := []string{voltrProgramDataAddress, adaptorProgramDataAddress}
	images, err := c.readProgramIdentityAccounts(ctx, programs, programHeaderLength)
	if err != nil {
		return nil, err
	}
	if full {
		// Full ProgramData images are read one address at a time: the pinned
		// Voltr image is over a megabyte and batch responses are size-capped.
		for _, address := range programDataAddresses {
			more, err := c.readProgramIdentityAccounts(ctx, []string{address}, 0)
			if err != nil {
				return nil, err
			}
			images = append(images, more...)
		}
		return images, nil
	}
	dataImages, err := c.readProgramIdentityAccounts(ctx, programDataAddresses, programDataHeaderLength)
	if err != nil {
		return nil, err
	}
	return append(images, dataImages...), nil
}

// readProgramIdentityAccounts reads one base64 account batch. A length of zero
// or less asks the node for the whole account instead of a data slice.
func (c *RPCClient) readProgramIdentityAccounts(ctx context.Context, addresses []string, length int) ([]programIdentityImage, error) {
	var result struct {
		Value []*struct {
			Owner      string          `json:"owner"`
			Lamports   uint64          `json:"lamports"`
			Executable bool            `json:"executable"`
			Data       json.RawMessage `json:"data"`
		} `json:"value"`
	}
	options := map[string]any{"commitment": "confirmed", "encoding": "base64"}
	if length > 0 {
		options["dataSlice"] = map[string]any{"offset": 0, "length": length}
	}
	if err := c.call(ctx, "getMultipleAccounts", []any{addresses, options}, &result); err != nil {
		return nil, confirmedObservationUnavailable(err)
	}
	if len(result.Value) != len(addresses) {
		return nil, confirmedObservationUnavailable(fmt.Errorf("incoherent program identity response"))
	}
	images := make([]programIdentityImage, 0, len(addresses))
	for index, value := range result.Value {
		if value == nil {
			continue
		}
		var encoded []string
		if err := json.Unmarshal(value.Data, &encoded); err != nil || len(encoded) != 2 || encoded[1] != "base64" {
			return nil, confirmedObservationUnavailable(fmt.Errorf("program identity account %s has invalid encoding", addresses[index]))
		}
		data, err := base64.StdEncoding.DecodeString(encoded[0])
		if err != nil {
			return nil, confirmedObservationUnavailable(fmt.Errorf("decode program identity account %s: %w", addresses[index], err))
		}
		images = append(images, programIdentityImage{
			Address: addresses[index], Owner: value.Owner, Lamports: value.Lamports,
			Executable: value.Executable, Data: data,
		})
	}
	return images, nil
}

func (c *RPCClient) LatestBlockhash(ctx context.Context) (LatestBlockhash, error) {
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight int64  `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getLatestBlockhash", []any{map[string]string{"commitment": "confirmed"}}, &result); err != nil {
		return LatestBlockhash{}, confirmedObservationUnavailable(err)
	}
	if result.Context.Slot <= 0 || result.Value.Blockhash == "" || result.Value.LastValidBlockHeight <= 0 {
		return LatestBlockhash{}, confirmedObservationUnavailable(fmt.Errorf("invalid confirmed blockhash response"))
	}
	return LatestBlockhash{Blockhash: result.Value.Blockhash, LastValidBlockHeight: result.Value.LastValidBlockHeight}, nil
}

// SimulateSignedTransaction verifies the exact signed bytes that would later
// be persisted. It never replaces the blockhash and requires signature checks.
func (c *RPCClient) SimulateSignedTransaction(ctx context.Context, signedWire []byte) (SimulationResult, error) {
	if len(signedWire) == 0 {
		return SimulationResult{}, fmt.Errorf("signed transaction wire is required")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Err           json.RawMessage `json:"err"`
			Logs          []string        `json:"logs"`
			UnitsConsumed uint64          `json:"unitsConsumed"`
		} `json:"value"`
	}
	encoded := base64.StdEncoding.EncodeToString(signedWire)
	err := c.call(ctx, "simulateTransaction", []any{encoded, map[string]any{
		"commitment": "confirmed", "encoding": "base64", "sigVerify": true,
		"replaceRecentBlockhash": false,
	}}, &result)
	if err != nil {
		return SimulationResult{}, err
	}
	if result.Context.Slot <= 0 || (len(result.Value.Err) > 0 && string(result.Value.Err) != "null") {
		logTail := result.Value.Logs
		if len(logTail) > 8 {
			logTail = logTail[len(logTail)-8:]
		}
		return SimulationResult{}, fmt.Errorf("signed transaction simulation failed: slot=%d err=%s log_tail=%q", result.Context.Slot, string(result.Value.Err), strings.Join(logTail, " | "))
	}
	return SimulationResult{Slot: result.Context.Slot, UnitsConsumed: result.Value.UnitsConsumed, Logs: append([]string(nil), result.Value.Logs...)}, nil
}

// SendSignedTransactionOnce submits exactly the persisted wire with RPC retries
// disabled. Callers must durably record broadcast_intent before invoking it.
func (c *RPCClient) SendSignedTransactionOnce(ctx context.Context, signedWire []byte, expectedSignature string) (string, error) {
	if len(signedWire) == 0 || expectedSignature == "" {
		return "", fmt.Errorf("persisted signed wire and signature are required")
	}
	var signature string
	encoded := base64.StdEncoding.EncodeToString(signedWire)
	err := c.call(ctx, "sendTransaction", []any{encoded, map[string]any{
		"encoding": "base64", "preflightCommitment": "confirmed", "skipPreflight": false, "maxRetries": 0,
	}}, &signature)
	if err != nil {
		return "", err
	}
	if signature != expectedSignature {
		return "", fmt.Errorf("RPC returned a different transaction signature")
	}
	return signature, nil
}

type signatureStatusRow struct {
	Slot               int64           `json:"slot"`
	Err                json.RawMessage `json:"err"`
	ConfirmationStatus string          `json:"confirmationStatus"`
}

// signatureObservation is the settled invariant shared by the singular and the
// batch reader: a processed-only result can still be forked away, so it proves
// nothing about failure or success and may never drive a terminal transition.
// Only a settled observation reports Failed; `Confirmed` additionally excludes
// a settled on-chain error.
func signatureObservation(row *signatureStatusRow) SignatureObservation {
	settled := row.Slot > 0 && (row.ConfirmationStatus == "confirmed" || row.ConfirmationStatus == "finalized")
	failed := settled && len(row.Err) > 0 && string(row.Err) != "null"
	return SignatureObservation{
		Found: true, Confirmed: settled && !failed,
		Finalized: settled && row.ConfirmationStatus == "finalized",
		Settled:   settled, ProcessedOnly: !settled,
		ConfirmationSlot: row.Slot, Failed: failed,
	}
}

func (c *RPCClient) SignatureStatus(ctx context.Context, signature string) (SignatureObservation, error) {
	if signature == "" {
		return SignatureObservation{}, fmt.Errorf("transaction signature is required")
	}
	var result struct {
		Value []*signatureStatusRow `json:"value"`
	}
	if err := c.call(ctx, "getSignatureStatuses", []any{[]string{signature}, map[string]bool{"searchTransactionHistory": true}}, &result); err != nil {
		return SignatureObservation{}, err
	}
	if len(result.Value) != 1 {
		return SignatureObservation{}, fmt.Errorf("signature status response has wrong cardinality")
	}
	if result.Value[0] == nil {
		return SignatureObservation{Found: false}, nil
	}
	return signatureObservation(result.Value[0]), nil
}

// FinalizedSignatureStatus re-reads a signature before an unreadable failure
// receipt may terminate an operation durably. getSignatureStatuses reports the
// highest confirmation level the node has observed, so only an observation
// with Finalized - or a settled success - proves the failure survived fork
// risk; a confirmed-only or absent signature keeps the operation observing.
func (c *RPCClient) FinalizedSignatureStatus(ctx context.Context, signature string) (SignatureObservation, error) {
	return c.SignatureStatus(ctx, signature)
}

// ConfirmedTransaction reads the immutable receipt for the exact persisted
// signature. Reconciliation must use these transaction-scoped pre/post token
// balances rather than a later account read that can include unrelated user
// deposits or claims.
func (c *RPCClient) ConfirmedTransaction(ctx context.Context, signature string) (ConfirmedTransactionEvidence, error) {
	return c.transactionReceipt(ctx, signature, "confirmed")
}

func (c *RPCClient) FinalizedTransaction(ctx context.Context, signature string) (ConfirmedTransactionEvidence, error) {
	return c.transactionReceipt(ctx, signature, "finalized")
}

func (c *RPCClient) transactionReceipt(ctx context.Context, signature, commitment string) (ConfirmedTransactionEvidence, error) {
	if signature == "" {
		return ConfirmedTransactionEvidence{}, fmt.Errorf("transaction signature is required")
	}
	type tokenBalance struct {
		AccountIndex uint16 `json:"accountIndex"`
		Mint         string `json:"mint"`
		Owner        string `json:"owner"`
		ProgramID    string `json:"programId"`
		Amount       struct {
			Amount string `json:"amount"`
		} `json:"uiTokenAmount"`
	}
	var result struct {
		Slot int64 `json:"slot"`
		Meta struct {
			Err               json.RawMessage `json:"err"`
			PreTokenBalances  []tokenBalance  `json:"preTokenBalances"`
			PostTokenBalances []tokenBalance  `json:"postTokenBalances"`
			LogMessages       []string        `json:"logMessages"`
			LoadedAddresses   struct {
				Writable []string `json:"writable"`
				Readonly []string `json:"readonly"`
			} `json:"loadedAddresses"`
			ReturnData *struct {
				ProgramID string   `json:"programId"`
				Data      []string `json:"data"`
			} `json:"returnData"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []string `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	if err := c.call(ctx, "getTransaction", []any{signature, map[string]any{
		"commitment": commitment, "encoding": "json", "maxSupportedTransactionVersion": 0,
	}}, &result); err != nil {
		return ConfirmedTransactionEvidence{}, err
	}
	if result.Slot <= 0 || (len(result.Meta.Err) > 0 && string(result.Meta.Err) != "null") || len(result.Transaction.Message.AccountKeys) == 0 {
		return ConfirmedTransactionEvidence{}, fmt.Errorf("confirmed transaction receipt is invalid")
	}
	accountKeys := append([]string(nil), result.Transaction.Message.AccountKeys...)
	accountKeys = append(accountKeys, result.Meta.LoadedAddresses.Writable...)
	accountKeys = append(accountKeys, result.Meta.LoadedAddresses.Readonly...)
	decodeBalances := func(rows []tokenBalance) ([]TransactionTokenBalance, error) {
		balances := make([]TransactionTokenBalance, len(rows))
		seen := make(map[string]struct{}, len(rows))
		for index, row := range rows {
			if int(row.AccountIndex) >= len(accountKeys) || row.Mint == "" || row.Owner == "" ||
				(row.ProgramID != classicTokenProgram && row.ProgramID != token2022Program) {
				return nil, fmt.Errorf("transaction token-balance identity is incomplete")
			}
			address := accountKeys[row.AccountIndex]
			if _, duplicate := seen[address]; duplicate {
				return nil, fmt.Errorf("transaction token-balance address is duplicated")
			}
			seen[address] = struct{}{}
			raw, err := strconv.ParseUint(row.Amount.Amount, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("transaction token amount is invalid")
			}
			balances[index] = TransactionTokenBalance{
				Address: address, OwnerProgram: row.ProgramID, Mint: row.Mint, Authority: row.Owner, Raw: raw,
			}
		}
		return balances, nil
	}
	pre, err := decodeBalances(result.Meta.PreTokenBalances)
	if err != nil {
		return ConfirmedTransactionEvidence{}, err
	}
	post, err := decodeBalances(result.Meta.PostTokenBalances)
	if err != nil {
		return ConfirmedTransactionEvidence{}, err
	}
	evidence := ConfirmedTransactionEvidence{
		Finalized: commitment == "finalized",
		Signature: signature, Slot: result.Slot, PreTokenBalances: pre, PostTokenBalances: post,
		Logs: append([]string(nil), result.Meta.LogMessages...),
	}
	if result.Meta.ReturnData != nil {
		if result.Meta.ReturnData.ProgramID == "" || len(result.Meta.ReturnData.Data) != 2 ||
			result.Meta.ReturnData.Data[1] != "base64" {
			return ConfirmedTransactionEvidence{}, fmt.Errorf("transaction return data is invalid")
		}
		if _, err := base64.StdEncoding.DecodeString(result.Meta.ReturnData.Data[0]); err != nil {
			return ConfirmedTransactionEvidence{}, fmt.Errorf("transaction return data is invalid")
		}
		evidence.ReturnData = &ProgramReturnData{
			ProgramID: result.Meta.ReturnData.ProgramID, DataBase64: result.Meta.ReturnData.Data[0],
		}
	}
	return evidence, nil
}

// FailedTransactionEvidence reads the immutable failure receipt for the exact
// persisted signature. The success-path receipt readers reject a non-null
// meta.err; this reader keeps it, because classifying a failed broadcast needs
// the RPC error shape and the failing program's log lines. Only a finalized
// receipt is acceptable: a failure can only be classified once it provably
// cannot be forked away.
func (c *RPCClient) FailedTransactionEvidence(ctx context.Context, signature string) (ConfirmedFailureEvidence, error) {
	if signature == "" {
		return ConfirmedFailureEvidence{}, fmt.Errorf("transaction signature is required")
	}
	var result struct {
		Slot int64 `json:"slot"`
		Meta struct {
			Err         json.RawMessage `json:"err"`
			LogMessages []string        `json:"logMessages"`
		} `json:"meta"`
	}
	if err := c.call(ctx, "getTransaction", []any{signature, map[string]any{
		"commitment": "finalized", "encoding": "json", "maxSupportedTransactionVersion": 0,
	}}, &result); err != nil {
		return ConfirmedFailureEvidence{}, err
	}
	if result.Slot <= 0 || len(result.Meta.Err) == 0 || string(result.Meta.Err) == "null" {
		return ConfirmedFailureEvidence{}, fmt.Errorf("failed transaction receipt is unavailable")
	}
	return ConfirmedFailureEvidence{
		Slot: result.Slot, Err: append([]byte(nil), result.Meta.Err...),
		Logs: append([]string(nil), result.Meta.LogMessages...),
	}, nil
}

func (c *RPCClient) ConfirmedBlockHeight(ctx context.Context) (int64, error) {
	return c.blockHeight(ctx, "confirmed")
}

func (c *RPCClient) FinalizedBlockHeight(ctx context.Context) (int64, error) {
	return c.blockHeight(ctx, "finalized")
}

func (c *RPCClient) blockHeight(ctx context.Context, commitment string) (int64, error) {
	var height int64
	if err := c.call(ctx, "getBlockHeight", []any{map[string]string{"commitment": commitment}}, &height); err != nil {
		return 0, err
	}
	if height <= 0 {
		return 0, fmt.Errorf("confirmed block height unavailable")
	}
	return height, nil
}

func ConfirmedSlot(ctx context.Context, rpcURL string) (int64, error) {
	client, err := NewRPCClient(rpcURL)
	if err != nil {
		return 0, err
	}
	return client.ConfirmedSlot(ctx)
}
