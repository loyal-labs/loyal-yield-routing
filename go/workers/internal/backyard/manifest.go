package backyard

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// RouteManifest is the fact set a decision is taken on: the earnLanes
// registry, identified by SHA256 (the hash of every lane's chain facts, which
// binds decision evidence and NAV report digests to it), and the lane a flat
// vault's view describes before any selector entry orients it (the
// registry's first lane).
type RouteManifest struct {
	RuntimeActivation RuntimeActivation
	SHA256            string
}

type RuntimeActivation struct {
	SelectedLane string
}

func loadEmbeddedRouteManifest() (RouteManifest, error) { return manifestOf(earnLanes) }

func manifestOf(lanes []earnLane) (RouteManifest, error) {
	routes := make([]RuntimeRoute, len(lanes))
	for i, l := range lanes {
		routes[i] = l.route
	}
	facts, err := json.Marshal(routes)
	if err != nil {
		return RouteManifest{}, err
	}
	hash := sha256.Sum256(facts)
	return RouteManifest{RuntimeActivation: RuntimeActivation{SelectedLane: lanes[0].route.Lane}, SHA256: hex.EncodeToString(hash[:])}, nil
}

var errKaminoPacketUnavailable = fmt.Errorf("Kamino packet unavailable")

// kaminoPacketForRoute is lane's packet for leg, for amount, executed through
// the leg's installed policy.
func (m RouteManifest) kaminoPacketForRoute(policies installedPolicies, action Action, leg kaminoPrimeUSDCLeg, amount uint64, blockhash LatestBlockhash, lane string) (KaminoPrimeUSDCRequest, error) {
	if amount == 0 || blockhash.Blockhash == "" || blockhash.LastValidBlockHeight <= 0 {
		return KaminoPrimeUSDCRequest{}, errKaminoPacketUnavailable
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return KaminoPrimeUSDCRequest{}, errKaminoPacketUnavailable
	}
	discriminator := kaminoLegDiscriminator(leg)
	if discriminator == nil {
		return KaminoPrimeUSDCRequest{}, errKaminoPacketUnavailable
	}
	key, _ := kaminoPolicyLeg(route, leg)
	policy, err := policies.account(key)
	if err != nil {
		return KaminoPrimeUSDCRequest{}, err
	}
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	metas := map[kaminoPrimeUSDCLeg][]accountMeta{kaminoLegDeposit: deposit, kaminoLegBorrow: borrow, kaminoLegRepay: repay, kaminoLegWithdraw: withdraw}[leg]
	accounts := make(KaminoPrimeUSDCAccounts, len(metas))
	for i, item := range metas {
		accounts[i].Address, accounts[i].Signer, accounts[i].Writable = encodeBase58(item.key[:]), item.signer, item.writable
	}
	data := binary.LittleEndian.AppendUint64(append([]byte(nil), discriminator...), amount)
	request := KaminoPrimeUSDCRequest{Action: action, AmountRaw: amount, Policy: policy, Accounts: accounts, Data: data,
		RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: lane}
	if _, observedLeg, err := kaminoRouteInstruction(request, lane); err != nil || observedLeg != leg {
		return KaminoPrimeUSDCRequest{}, errKaminoPacketUnavailable
	}
	return request, nil
}

func (m RouteManifest) activeRuntimeRoute() (RuntimeRoute, error) {
	return runtimeRoute(m.RuntimeActivation.SelectedLane)
}
