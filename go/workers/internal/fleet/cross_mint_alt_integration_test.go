package fleet

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

// Registered SQL proof only: the typed source builder and concrete reader use
// synthetic chain account bytes. No SVM, provider or finalized-chain claim.
func TestCrossMintExternalRegisteredQueuePreservesV1AndBoundComposite(t *testing.T) {
	for _, mode := range []string{"v1", "external"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx := crossMintCapacityFixtureStore(t)
			l, original := waitingIdentity(t, ctx, s, fmt.Sprintf("external-queue-%s-%d", mode, time.Now().UnixNano()))
			manifest := original
			covered := []string{l.SourceReserve, l.SourceLiquidityMint, l.VaultPubkey, l.PolicyAccount}
			if mode == "external" {
				data := make([]byte, 56+32*len(covered))
				binary.LittleEndian.PutUint32(data, 1)
				binary.LittleEndian.PutUint64(data[4:12], math.MaxUint64)
				binary.LittleEndian.PutUint64(data[12:20], 90)
				for i, address := range covered {
					fixtureKey(t, data, 56+32*i, address)
				}
				r := &Revalidator{rpc: crossMintPrepareRPC(t, func(method string, params []json.RawMessage) any {
					var options struct {
						Commitment     string
						MinContextSlot int64
					}
					_ = json.Unmarshal(params[1], &options)
					if method != "getMultipleAccounts" || options.Commitment != "finalized" || options.MinContextSlot != 100 {
						t.Fatal("manifest did not use actual finalized reader")
					}
					return map[string]any{"context": map[string]any{"slot": 101}, "value": []any{map[string]any{"owner": altProgram, "lamports": 1000, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}}}
				})}
				var err error
				manifest, err = r.bindFinalizedCrossMintALTManifest(context.Background(), original, []LookupTable{{Address: manifestKey(244), Addresses: covered, Active: true}}, 100)
				if err != nil {
					t.Fatal(err)
				}
			}
			seedWaitingCatalog(t, ctx, s, l.Cluster, manifest.SharedAddresses)
			p := waitingManifestPreparation(manifest)
			var err error
			p.RequirementsFingerprint, err = ALTManifestRequirementsFingerprint(&manifest)
			if err != nil {
				t.Fatal(err)
			}
			if (mode == "v1") != (p.RequirementsFingerprint == original.Fingerprint) {
				t.Fatal("v1 and actual composite identity conflated")
			}
			missing := []string{manifest.VaultAddresses[0].Address}
			if mode == "external" {
				forged := p
				forged.Manifest = &original // pure computed hash is not finalized provenance.
				if _, err := upsertWaitingFixture(ctx, s, l, forged, missing); err == nil {
					t.Fatal("caller-computed composite forged source binder")
				}
				var count int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_requests WHERE cluster=$1`, l.Cluster).Scan(&count); err != nil || count != 0 {
					t.Fatalf("forged request leaked durable writes: count=%d err=%v", count, err)
				}
			}
			id, err := upsertWaitingFixture(ctx, s, l, p, missing)
			if err != nil {
				t.Fatal(err)
			}
			repeated, err := upsertWaitingFixture(ctx, s, l, p, missing)
			if err != nil || repeated != id {
				t.Fatalf("exact source request not idempotent: %d %d %v", id, repeated, err)
			}
			var fp string
			var sharedCount, vaultCount int
			var sealed bool
			if err := s.pool.QueryRow(ctx, `SELECT requirements_fingerprint,desired_shared_address_count,desired_vault_address_count,sealed_at IS NOT NULL FROM loyal_yield.lookup_table_provisioning_requests WHERE id=$1`, id).Scan(&fp, &sharedCount, &vaultCount, &sealed); err != nil || fp != p.RequirementsFingerprint || sharedCount != len(manifest.SharedAddresses) || vaultCount != len(manifest.VaultAddresses) || !sealed {
				t.Fatalf("queue lost exact full sealed composite: fp=%s counts=%d/%d sealed=%t err=%v", fp, sharedCount, vaultCount, sealed, err)
			}
			if mode == "external" {
				var count int
				if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.lookup_table_provisioning_request_addresses WHERE request_id=$1 AND address=ANY($2)`, id, covered).Scan(&count); err != nil || count != 0 {
					t.Fatalf("proven external keys became managed demand: count=%d err=%v", count, err)
				}
				foreign := l
				foreign.SourceReserve = manifestKey(88)
				if _, err := upsertWaitingFixture(ctx, s, foreign, p, missing); err == nil {
					t.Fatal("filtered vectors erased original source identity binding")
				}
			}
		})
	}
}
