package fleetexec

import (
	"math"
	"testing"
)

func TestLookupLegacyOriginalGrowthIntent(t *testing.T) {
	op := LookupOperation{Intent: LookupIntent{Kind: LookupExtend, MutationEpoch: 4, Prefix: []string{"old", "new-a", "new-b"}, Extension: []string{"new-a", "new-b"}}, PhysicalMutationEpoch: 5}
	i, err := lookupLegacyOriginalIntent(op)
	if err != nil || !lookupSameAddresses(i.Prefix, []string{"old"}) {
		t.Fatalf("original prefix: %+v, %v", i, err)
	}
	if !lookupSameAddresses(op.Intent.Prefix, []string{"old", "new-a", "new-b"}) {
		t.Fatal("original projection changed")
	}
	for _, mutate := range []func(*LookupOperation){
		func(o *LookupOperation) { o.PhysicalMutationEpoch = 6 },
		func(o *LookupOperation) { o.Intent.Prefix = []string{"old", "new-b", "new-a"} },
		func(o *LookupOperation) { o.Intent.Kind = LookupDeactivate },
		func(o *LookupOperation) { o.Intent.Extension = nil },
		func(o *LookupOperation) { o.Intent.MutationEpoch = math.MaxInt64 },
	} {
		bad := op
		mutate(&bad)
		if _, err := lookupLegacyOriginalIntent(bad); err == nil {
			t.Fatal("accepted unrelated physical projection")
		}
	}
}
