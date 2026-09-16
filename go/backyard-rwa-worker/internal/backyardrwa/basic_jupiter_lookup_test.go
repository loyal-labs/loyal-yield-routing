package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func exportedBasicJupiterRecord(t *testing.T, lane, leg string) basicMessageRecord {
	t.Helper()
	data, err := os.ReadFile("../../../../docs/evidence/backyard-rwa-basic/go-messages-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var exported struct {
		Messages []basicMessageRecord `json:"messages"`
	}
	if err := json.Unmarshal(data, &exported); err != nil {
		t.Fatal(err)
	}
	for _, record := range exported.Messages {
		if record.Lane == lane && record.Leg == leg && record.Kind == "jupiter" {
			return record
		}
	}
	t.Fatalf("exported basic message %s %s is missing", lane, leg)
	return basicMessageRecord{}
}

func basicSwapActionForLeg(leg string) Action {
	switch leg {
	case "USDC->PRIME", "USDC->syrupUSDC", "USDC->ONyc":
		return SwapStableToCollateralStep
	case "PRIME->USDC", "syrupUSDC->USDC", "ONyc->USDC":
		return SwapCollateralToStableStep
	}
	return ""
}

// quotedJupiterLookupHints returns the addressLookupTableAddresses Jupiter
// returned beside each retained instruction.
func quotedJupiterLookupHints(t *testing.T, key string) []string {
	t.Helper()
	data, err := os.ReadFile(basicJupiterFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var evidence struct {
		Rows []struct {
			Key          string
			LookupTables []string
		}
	}
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	for _, row := range evidence.Rows {
		if row.Key != key {
			continue
		}
		if len(row.LookupTables) == 0 || len(row.LookupTables) > 4 {
			t.Fatalf("quoted hints for %s are unreviewable", key)
		}
		return row.LookupTables
	}
	t.Fatalf("header evidence has no %s row", key)
	return nil
}

// legacyMessageKeys decodes the exported legacy packet's account keys.
func legacyMessageKeys(t *testing.T, messageBase64 string) []string {
	t.Helper()
	message, err := base64.StdEncoding.Strict().DecodeString(messageBase64)
	if err != nil || len(message) < 4 || message[0] != 1 {
		t.Fatalf("exported legacy message is malformed: %v", err)
	}
	count := int(message[3])
	if 4+32*count > len(message) {
		t.Fatal("exported legacy message truncates its account keys")
	}
	keys := make([]string, 0, count)
	for index := 0; index < count; index++ {
		start := 4 + 32*index
		keys = append(keys, encodeBase58(message[start:start+32]))
	}
	return keys
}

// retainedOrReconstructedLookupTables resolves the quoted hints the way the
// runtime does, from chain-observed table accounts. The Prime sibling capture
// holds the exact chain bytes for its hints. The Maple and OnRe hint tables
// were never retained, so their bodies are reconstructed offline over the venue
// accounts the recorded instruction already pins; authority accounts stay
// static exactly as they would outside a Jupiter-owned table. Chain contents
// remain the runtime authority: prepare and revalidate re-read and re-hash
// them on every build.
func retainedOrReconstructedLookupTables(t *testing.T, hints, messageKeys, authorities []string) []LookupTableSnapshot {
	t.Helper()
	retained := readRetainedJupiterLookups(t, "prime-sibling-lookup-review-2026-09-05.json", 8)
	found := map[string]LookupTableSnapshot{}
	for _, table := range retained {
		for _, hint := range hints {
			if table.Address == hint {
				found[hint] = table
			}
		}
	}
	if len(found) == len(hints) {
		tables := make([]LookupTableSnapshot, 0, len(hints))
		for _, hint := range hints {
			tables = append(tables, found[hint])
		}
		return tables
	}
	if len(found) != 0 {
		t.Fatal("partially retained hint set cannot be reconstructed honestly")
	}
	static := map[string]bool{bridgeDelegate: true, bridgeSquadsProgram: true, bridgeVault: true, bridgeSquadsATA: true}
	for _, authority := range authorities {
		static[authority] = true
	}
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		route, err := runtimeRoute(lane)
		if err != nil {
			t.Fatal(err)
		}
		static[route.CollateralCustody] = true
	}
	bodies := make([][]publicKey, len(hints))
	loaded := 0
	for _, key := range messageKeys {
		if static[key] {
			continue
		}
		entry, err := decodeBase58PublicKey(key)
		if err != nil {
			t.Fatalf("recorded key %s is malformed", key)
		}
		bodies[loaded%len(bodies)] = append(bodies[loaded%len(bodies)], entry)
		loaded++
	}
	if loaded == 0 {
		t.Fatal("no venue keys were available to reconstruct a table body")
	}
	tables := make([]LookupTableSnapshot, 0, len(hints))
	for index, hint := range hints {
		if _, err := decodeBase58PublicKey(hint); err != nil {
			t.Fatalf("hint %s is malformed", hint)
		}
		body := make([]byte, 56, 56+32*len(bodies[index]))
		binary.LittleEndian.PutUint32(body[0:4], 1)
		binary.LittleEndian.PutUint64(body[4:12], ^uint64(0))
		for _, entry := range bodies[index] {
			body = append(body, entry[:]...)
		}
		table := LookupTableSnapshot{Address: hint, Owner: addressLookupTableProgram, Lamports: 56960640, Data: body, ObservedSlot: 1_000_000}
		if _, err := decodeMessageLookupTable(table); err != nil {
			t.Fatalf("reconstructed table is invalid: %v", err)
		}
		tables = append(tables, table)
	}
	return tables
}

func basicJupiterRequestFromExport(t *testing.T, lane, leg string) (JupiterSwapRequest, basicMessageRecord) {
	t.Helper()
	record := exportedBasicJupiterRecord(t, lane, leg)
	action := basicSwapActionForLeg(leg)
	if len(record.Instructions) != 2 || record.Instructions[0].Kind != "squads_execute_transaction_sync" ||
		record.Instructions[1].Kind != "jupiter_shared_accounts_route" {
		t.Fatalf("exported %s %s lost its recorded instruction pair", lane, leg)
	}
	inner := record.Instructions[1]
	data, err := base64.StdEncoding.Strict().DecodeString(inner.DataBase64)
	if err != nil || len(data) < 19 {
		t.Fatalf("recorded inner instruction is malformed: %v", err)
	}
	accounts := make([]JupiterInstructionAccount, 0, len(inner.Accounts))
	for _, account := range inner.Accounts {
		accounts = append(accounts, JupiterInstructionAccount{Pubkey: account.Pubkey, IsSigner: account.IsSigner, IsWritable: account.IsWritable})
	}
	amount := readU64(data[len(data)-19:])
	if amount != record.AmountRaw {
		t.Fatalf("recorded amount %d does not match instruction economics %d", record.AmountRaw, amount)
	}
	request := JupiterSwapRequest{Action: action, AmountRaw: amount, QuotedOutputRaw: readU64(data[len(data)-11:]),
		MinimumOutputRaw: readU64(data[len(data)-11:]), Policy: record.PolicyAccount, PolicyAccountDataSHA256: record.PolicyDataSHA256,
		PolicyConstraintIndex: record.ConstraintIndex, Instruction: JupiterSwapInstruction{ProgramID: inner.ProgramID, Accounts: accounts, Data: inner.DataBase64},
		RecentBlockhash: record.RecentBlockhash, LastValidBlockHeight: record.LastValidBlockHeight, RouteLane: lane}
	if !acceptsJupiterLookupHints(lane, action) {
		t.Fatalf("basic lane %s does not admit its quoted lookup hints", lane)
	}
	request.Instruction.LookupTableAddresses = quotedJupiterLookupHints(t, leg)
	return request, record
}

// decodeV0OuterInstruction reads the single Squads execute back out of a
// compiled v0 message. The Squads authority accounts must stay static, so their
// references are resolved from the static key list while venue accounts past
// them may legitimately resolve through a table.
func decodeV0OuterInstruction(t *testing.T, message []byte) ([]string, []string, []byte) {
	t.Helper()
	offset := 4
	staticCount, err := decodeShortVec(message, &offset)
	if err != nil || staticCount == 0 || 4+32*staticCount > len(message) {
		t.Fatalf("v0 message truncates its static keys: %v", err)
	}
	staticKeys := make([]string, 0, staticCount)
	for index := 0; index < staticCount; index++ {
		staticKeys = append(staticKeys, encodeBase58(message[offset:offset+32]))
		offset += 32
	}
	offset += 32 // recent blockhash
	instructions, err := decodeShortVec(message, &offset)
	if err != nil || instructions != 1 {
		t.Fatalf("v0 message does not carry exactly one instruction: %v", err)
	}
	programIndex, err := decodeShortVec(message, &offset)
	if err != nil || programIndex >= staticCount || staticKeys[programIndex] != bridgeSquadsProgram {
		t.Fatalf("v0 outer program drifted: %v", err)
	}
	accountsCount, err := decodeShortVec(message, &offset)
	if err != nil {
		t.Fatal(err)
	}
	accounts := make([]string, 0, 3)
	for index := 0; index < accountsCount; index++ {
		accountIndex, err := decodeShortVec(message, &offset)
		if err != nil {
			t.Fatal(err)
		}
		if index >= 3 {
			continue // venue accounts may legitimately resolve through a table
		}
		if accountIndex >= staticCount {
			t.Fatalf("v0 outer authority account %d moved into a lookup table", index)
		}
		accounts = append(accounts, staticKeys[accountIndex])
	}
	dataLength, err := decodeShortVec(message, &offset)
	if err != nil || offset+dataLength > len(message) {
		t.Fatalf("v0 message truncates its instruction data: %v", err)
	}
	return staticKeys, accounts, message[offset : offset+dataLength]
}

func TestOversizedBasicSwapLegsBuildV0FromQuotedLookupHints(t *testing.T) {
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		leg := map[string]string{PhaseOneLaneID: "USDC->PRIME", SelectedRouteID: "syrupUSDC->USDC", "OnRe/ONyc/USDC": "ONyc->USDC"}[lane]
		t.Run(lane, func(t *testing.T) {
			request, record := basicJupiterRequestFromExport(t, lane, leg)
			if record.SingleSignerPacketFits {
				t.Fatal("exported leg was not oversized")
			}
			message, err := CompileJupiterMessage(request)
			if err == nil || !strings.Contains(err.Error(), "unsigned message does not fit") {
				t.Fatalf("hinted request without retained tables compiled as %v", err)
			}
			messageKeys := legacyMessageKeys(t, record.MessageBase64)
			tables := retainedOrReconstructedLookupTables(t, request.Instruction.LookupTableAddresses, messageKeys, []string{record.PolicyAccount})
			request.LookupTables = tables
			message, err = CompileJupiterMessage(request)
			if err != nil || message[0] != 0x80 || message[1] != 1 {
				t.Fatalf("validated hints did not produce a v0 outer message: %v", err)
			}
			if len(message)+65 > solanaPacketBytes || len(message)+65 >= record.SingleSignerPacketBytes {
				t.Fatalf("v0 packet %d did not replace the %d byte legacy packet", len(message)+65, record.SingleSignerPacketBytes)
			}
			staticKeys, accounts, outerData := decodeV0OuterInstruction(t, message)
			for _, authority := range []string{record.PolicyAccount, bridgeSquadsProgram, bridgeDelegate} {
				found := false
				for _, key := range staticKeys {
					found = found || key == authority
				}
				if !found {
					t.Fatalf("v0 message offloaded authority account %s", authority)
				}
			}
			if base64.StdEncoding.EncodeToString(outerData) != record.Instructions[0].DataBase64 {
				t.Fatal("v0 outer instruction data left the recorded Squads execute")
			}
			if accounts[0] != record.PolicyAccount || accounts[1] != bridgeSquadsProgram || accounts[2] != bridgeDelegate {
				t.Fatalf("v0 outer authority accounts drifted: %v", accounts)
			}
			covered := map[string]bool{}
			for _, key := range staticKeys {
				covered[key] = true
			}
			for _, table := range tables {
				decoded, err := decodeMessageLookupTable(table)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range decoded.addresses {
					covered[encodeBase58(entry[:])] = true
				}
			}
			for _, key := range messageKeys {
				if !covered[key] {
					t.Fatalf("v0 message dropped recorded account %s", key)
				}
			}
			rpc, reads := lookupRPC(t, tables, nil, false)
			unprepared := request
			unprepared.LookupTables = nil
			prepared, err := prepareJupiterLookupTables(context.Background(), rpc, unprepared, tables[0].ObservedSlot)
			if err != nil || *reads != 1 || len(prepared.LookupTables) != len(tables) {
				t.Fatalf("basic lane preparation did not load the hinted chain tables: %v", err)
			}
			preparedMessage, err := CompileJupiterMessage(prepared)
			if err != nil || !bytes.Equal(message, preparedMessage) {
				t.Fatalf("fresh preparation changed the basic lane packet: %v", err)
			}
		})
	}
}

func TestFittingBasicSwapLegsKeepTheirLegacyPacket(t *testing.T) {
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		leg := map[string]string{PhaseOneLaneID: "PRIME->USDC", SelectedRouteID: "USDC->syrupUSDC", "OnRe/ONyc/USDC": "USDC->ONyc"}[lane]
		t.Run(lane, func(t *testing.T) {
			request, record := basicJupiterRequestFromExport(t, lane, leg)
			if !record.SingleSignerPacketFits {
				t.Fatal("exported leg was not a fitting legacy packet")
			}
			message, err := CompileJupiterMessage(request)
			if err != nil || message[0] != 1 {
				t.Fatalf("fitting basic lane leg did not stay legacy: %v", err)
			}
			if encoded := base64.StdEncoding.EncodeToString(message); encoded != record.MessageBase64 {
				t.Fatal("legacy packet left the exported bytes")
			}
			tables := retainedOrReconstructedLookupTables(t, request.Instruction.LookupTableAddresses, legacyMessageKeys(t, record.MessageBase64), []string{record.PolicyAccount})
			rpc, reads := lookupRPC(t, tables, nil, false)
			prepared, err := prepareJupiterLookupTables(context.Background(), rpc, request, tables[0].ObservedSlot)
			if err != nil || *reads != 0 || len(prepared.LookupTables) != 0 {
				t.Fatalf("fitting leg reached chain lookup preparation: %v", err)
			}
			again, err := CompileJupiterMessage(prepared)
			if err != nil || !bytes.Equal(message, again) {
				t.Fatalf("prepared fitting leg changed the legacy packet: %v", err)
			}
		})
	}
}

func TestBasicLaneLookupHintsStayScopedToSwapEdges(t *testing.T) {
	for _, lane := range []string{PhaseOneLaneID, SelectedRouteID, "OnRe/ONyc/USDC"} {
		if !acceptsJupiterLookupHints(lane, SwapStableToCollateralStep) || !acceptsJupiterLookupHints(lane, SwapCollateralToStableStep) {
			t.Fatalf("basic lane %s lost its swap hint admission", lane)
		}
		for _, action := range []Action{OpenRouteStep, DeleverRouteStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep, HoldManualRecovery} {
			if acceptsJupiterLookupHints(lane, action) {
				t.Fatalf("basic lane %s admitted hints for %s", lane, action)
			}
		}
	}
	if acceptsJupiterLookupHints(RouteID, SwapUSDCToPrimeStep) {
		t.Fatal("legacy Prime lane changed its hint admission")
	}
	for _, lane := range []string{"unknown/asset/debt", "AUTO/AUTO/PYUSD"} {
		if acceptsJupiterLookupHints(lane, SwapStableToCollateralStep) {
			t.Fatalf("lane %s gained unregistered hint admission", lane)
		}
	}
}
