package backyardrwa

import (
	"encoding/binary"
	"strings"
	"testing"
)

func navContext(slot int64) NAVSnapshotContext {
	return NAVSnapshotContext{
		Slot: slot, ReceiptFingerprint: "none",
		ManifestSHA256: strings.Repeat("a", 64), PolicyCatalogSHA256: strings.Repeat("b", 64),
	}
}

func custodyFixture(mint, authority [32]byte, amount uint64, token2022 bool) []byte {
	data := make([]byte, 165)
	copy(data[:32], mint[:])
	copy(data[32:64], authority[:])
	binary.LittleEndian.PutUint64(data[64:72], amount)
	data[108] = 1
	return data
}

func TestDecodeClassicAndToken2022Custodies(t *testing.T) {
	var mint, authority [32]byte
	mint[0], authority[0] = 1, 2
	classic, err := DecodeTokenCustody(classicTokenProgram, custodyFixture(mint, authority, 9, false), mint, authority)
	if err != nil || classic.Raw != 9 {
		t.Fatalf("classic=%+v err=%v", classic, err)
	}
	token2022, err := DecodeTokenCustody(token2022Program, custodyFixture(mint, authority, 11, true), mint, authority)
	if err != nil || token2022.Raw != 11 {
		t.Fatalf("token2022=%+v err=%v", token2022, err)
	}
	wrongMint := mint
	wrongMint[1] = 3
	if _, err := DecodeTokenCustody(token2022Program, custodyFixture(mint, authority, 11, true), wrongMint, authority); err == nil {
		t.Fatal("wrong mint accepted")
	}
	badExtension := append(custodyFixture(mint, authority, 11, true), 2)
	if _, err := DecodeTokenCustody(token2022Program, badExtension, mint, authority); err == nil {
		t.Fatal("Token-2022 extension accepted")
	}
}

func TestDecodeTokenCustodyRejectsFrozenAndExtraAuthority(t *testing.T) {
	var mint, authority [32]byte
	mint[0], authority[0] = 1, 2
	frozen := custodyFixture(mint, authority, 1, false)
	frozen[108] = 2
	if _, err := DecodeTokenCustody(classicTokenProgram, frozen, mint, authority); err == nil {
		t.Fatal("frozen custody accepted")
	}
	delegated := custodyFixture(mint, authority, 1, false)
	delegated[72] = 1
	if _, err := DecodeTokenCustody(classicTokenProgram, delegated, mint, authority); err == nil {
		t.Fatal("delegated custody accepted")
	}
}

func TestConservativeValuationRoundingAndObligationBound(t *testing.T) {
	asset, err := ValueRawUSDC(1, 1, 15, false)
	if err != nil || asset != 1 {
		t.Fatalf("asset=%d err=%v", asset, err)
	}
	liability, err := ValueRawUSDC(1, 1, 15, true)
	if err != nil || liability != 2 {
		t.Fatalf("liability=%d err=%v", liability, err)
	}
	if err := ValidateSingleObligation([]ObligationNAV{{Address: "one", Recognized: true, Nonzero: true}}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSingleObligation([]ObligationNAV{{Address: "one", Recognized: true, Nonzero: true}, {Address: "two", Recognized: true, Nonzero: true}}); err == nil {
		t.Fatal("multiple active obligations accepted")
	}
}

func TestNAVAssetsMinusLiabilitiesAndDigest(t *testing.T) {
	c := []NAVComponent{{Account: "classic", Owner: "Tokenkeg", Raw: 10, Slot: 7, Known: true}, {Account: "t22", Owner: "Tokenz", Raw: 11, Slot: 7, Known: true}, {Account: "debt", Owner: "Tokenkeg", Raw: 4, Slot: 7, Known: true, Liability: true}, {Account: "classic", Owner: "Tokenkeg", Raw: 10, Slot: 7, Known: true}}
	got, e := ComputeNAV(navContext(7), c)
	if e != nil || got.Raw != 17 || len(got.SnapshotDigest) != 64 {
		t.Fatalf("%+v %v", got, e)
	}
	reverse, _ := ComputeNAV(navContext(7), []NAVComponent{c[2], c[1], c[0]})
	if got.SnapshotDigest != reverse.SnapshotDigest {
		t.Fatal("nondeterministic digest")
	}
}
func TestNAVRejectsInvalidAndUnderflow(t *testing.T) {
	if _, e := ComputeNAV(navContext(7), []NAVComponent{{Account: "x", Owner: "Token", Raw: 1, Slot: 7}}); e == nil {
		t.Fatal("unknown accepted")
	}
	if _, e := ComputeNAV(navContext(7), []NAVComponent{{Account: "x", Owner: "Token", Raw: 1, Slot: 8, Known: true}}); e == nil {
		t.Fatal("mixed slot accepted")
	}
	if _, e := ComputeNAV(navContext(7), []NAVComponent{{Account: "x", Owner: "Token", Raw: 1, Slot: 7, Known: true, Liability: true}}); e == nil {
		t.Fatal("underflow accepted")
	}
	if _, e := ComputeNAV(navContext(7), []NAVComponent{
		{Account: "x", Owner: "Token", Raw: 1, Slot: 7, Known: true},
		{Account: "x", Owner: "Token", Raw: 2, Slot: 7, Known: true},
	}); e == nil {
		t.Fatal("conflicting alias accepted")
	}
}
