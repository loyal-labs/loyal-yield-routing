package fleetexec

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLookupRPCSanitizesQueryCredentialAndRedirectErrors(t *testing.T) {
	for _, kind := range []string{"provider", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			followed := false
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true; w.WriteHeader(200) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "redirect" {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				_, _ = w.Write([]byte(`{"error":{"code":-32000,"message":"private-secret-credential"}}`))
			}))
			defer server.Close()
			rpc, err := NewLookupRPC(server.URL+"?api-key=private-secret-credential", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = rpc.LookupBalance(context.Background(), readLookupFixture(t).Manager)
			if err == nil || strings.Contains(err.Error(), "private-secret") || followed {
				t.Fatalf("credential rendered or redirect followed: %v %v", err, followed)
			}
		})
	}
}
func TestLookupSnapshotDecodesOfficialPaddedSlotHashes(t *testing.T) {
	f := readLookupFixture(t)
	var account struct {
		Owner      string
		Lamports   uint64
		Data       string
		Executable bool
	}
	if err := json.Unmarshal(f.Accounts["SysvarS1otHashes111111111111111111111111111"], &account); err != nil {
		t.Fatal(err)
	}
	falseValue := false
	makeHashes := func(data string) *lookupRPCAccount {
		return &lookupRPCAccount{Owner: account.Owner, Lamports: &account.Lamports, Executable: &falseValue, Data: []json.RawMessage{json.RawMessage(`"` + data + `"`), json.RawMessage(`"base64"`)}}
	}
	out, err := decodeLookupSnapshot(f.Table, 1000, nil, makeHashes(account.Data))
	if err != nil || !out.Absent || len(out.SlotHashes) != 2 || out.SlotHashes[0] != 999 {
		t.Fatalf("actual official fixture: %+v %v", out, err)
	}
	d, err := base64.StdEncoding.DecodeString(account.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"future", "padding", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			copyData := append([]byte(nil), d...)
			if kind == "future" {
				binary.LittleEndian.PutUint64(copyData[8:], 1001)
			}
			if kind == "padding" {
				copyData[len(copyData)-1] = 1
			}
			a := makeHashes(base64.StdEncoding.EncodeToString(copyData))
			if kind == "wrong-owner" {
				a.Owner = f.Manager
			}
			if _, err := decodeLookupSnapshot(f.Table, 1000, nil, a); err == nil {
				t.Fatal("malformed cooldown proof accepted")
			}
		})
	}
}
