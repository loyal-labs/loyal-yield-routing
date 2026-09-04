package fleet

import (
	"encoding/binary"
	"testing"
	"time"
)

func testIdentity(seed byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return encodeBase58(raw)
}

func reserveFixture(identity ReserveIdentity, available, borrowed uint64) Account {
	data := make([]byte, reserveLength)
	copy(data[:8], reserveDiscriminator[:])
	binary.LittleEndian.PutUint64(data[8:16], 1)
	binary.LittleEndian.PutUint64(data[16:24], 900)
	market, _ := decodePublicKey(identity.Market)
	copy(data[32:64], market[:])
	mint, _ := decodePublicKey(identity.Mint)
	copy(data[128:160], mint[:])
	binary.LittleEndian.PutUint64(data[224:232], available)
	putScaledInteger(data[232:248], borrowed)
	binary.LittleEndian.PutUint64(data[272:280], 6)
	config := data[reserveConfigOffset:]
	config[14] = 10
	for index := 0; index < 11; index++ {
		offset := 64 + index*8
		binary.LittleEndian.PutUint32(config[offset:offset+4], uint32(index*1_000))
		binary.LittleEndian.PutUint32(config[offset+4:offset+8], uint32(index*200))
	}
	return Account{Address: identity.Address, Owner: KaminoProgram, Lamports: 1, Data: data}
}

func putScaledInteger(output []byte, value uint64) {
	binary.LittleEndian.PutUint64(output[:8], value<<60)
	binary.LittleEndian.PutUint64(output[8:16], value>>4)
}

func TestDecodeKaminoReserveMatchesFrozenLayout(t *testing.T) {
	identity := ReserveIdentity{Address: testIdentity(3), Market: testIdentity(40), Mint: USDCMint}
	state, err := DecodeKaminoReserve(reserveFixture(identity, 50_000_000_000_000, 50_000_000_000_000), identity, 1_000, 400*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if state.Slot != 1_000 || state.LastUpdateSlot != 900 || state.TotalSupplyUSDMicros != 100_000_000_000_000 {
		t.Fatalf("unexpected state: %+v", state)
	}
	if state.SupplyAPYBPS < 570 || state.SupplyAPYBPS > 590 {
		t.Fatalf("unexpected APY %d bps", state.SupplyAPYBPS)
	}
	if state.EconomicLifetimeMillis != 560_000 {
		t.Fatalf("unexpected economic lifetime %dms", state.EconomicLifetimeMillis)
	}
	if state.DataHash == "" {
		t.Fatal("missing immutable data hash")
	}
}

func TestDecodeKaminoReserveRejectsIdentityAndLayoutDrift(t *testing.T) {
	identity := ReserveIdentity{Address: testIdentity(3), Market: testIdentity(40), Mint: USDCMint}
	account := reserveFixture(identity, 1_000_000, 1_000_000)
	drifted := identity
	drifted.Market = testIdentity(80)
	if _, err := DecodeKaminoReserve(account, drifted, 1_000, 400*time.Millisecond); err == nil {
		t.Fatal("market drift was accepted")
	}
	account.Data[24] = 1
	staleTarget, err := DecodeKaminoReserve(account, identity, 1_000, 400*time.Millisecond)
	if err != nil || !staleTarget.LastUpdateStale {
		t.Fatalf("structurally valid stale target evidence was not decoded: state=%+v err=%v", staleTarget, err)
	}
	staleSource, err := DecodeKaminoSourceReserve(account, identity, 1_000, 400*time.Millisecond)
	if err != nil || !staleSource.LastUpdateStale {
		t.Fatalf("explicitly stale source-only reserve was rejected: state=%+v err=%v", staleSource, err)
	}
	account = reserveFixture(identity, 1_000_000, 1_000_000)
	binary.LittleEndian.PutUint64(account.Data[16:24], 1_001)
	if _, err := DecodeKaminoReserve(account, identity, 1_000, 400*time.Millisecond); err == nil {
		t.Fatal("future economic evidence was accepted")
	}
	binary.LittleEndian.PutUint64(account.Data[16:24], 1)
	expiring, err := DecodeKaminoReserve(account, identity, 2_400, 400*time.Millisecond)
	if err != nil || expiring.EconomicLifetimeMillis >= minimumPublicationLifetime.Milliseconds() {
		t.Fatalf("expiring evidence was not preserved for planner exclusion: state=%+v err=%v", expiring, err)
	}
	account.Data = account.Data[:100]
	if _, err := DecodeKaminoReserve(account, identity, 1_000, 400*time.Millisecond); err == nil {
		t.Fatal("layout drift was accepted")
	}
}
