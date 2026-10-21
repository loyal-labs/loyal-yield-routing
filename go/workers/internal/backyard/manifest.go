package backyard

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type RouteManifest struct {
	RuntimeActivation RuntimeActivation
	SHA256            string
	vault             *Vault
}

type RuntimeActivation struct {
	SelectedLane string
}

func manifestOf(v *Vault) (RouteManifest, error) {
	facts, err := json.Marshal(v.routes)
	if err != nil {
		return RouteManifest{}, err
	}
	hash := sha256.Sum256(facts)
	return RouteManifest{RuntimeActivation: RuntimeActivation{SelectedLane: v.routes[0].Lane}, SHA256: hex.EncodeToString(hash[:]), vault: v}, nil
}

func (m RouteManifest) route(lane string) (RuntimeRoute, error) { return m.vault.route(lane) }

var errKaminoPacketUnavailable = fmt.Errorf("Kamino packet unavailable")

func (m RouteManifest) kaminoPacketForRoute(policies installedPolicies, action Action, leg kaminoLeg, amount uint64, blockhash LatestBlockhash, lane string) (KaminoLegRequest, error) {
	if amount == 0 || blockhash.Blockhash == "" || blockhash.LastValidBlockHeight <= 0 {
		return KaminoLegRequest{}, errKaminoPacketUnavailable
	}
	route, err := m.route(lane)
	if err != nil {
		return KaminoLegRequest{}, errKaminoPacketUnavailable
	}
	discriminator := kaminoLegDiscriminator(leg)
	if discriminator == nil {
		return KaminoLegRequest{}, errKaminoPacketUnavailable
	}
	key, _ := kaminoPolicyLeg(route, leg)
	policy, err := policies.account(key)
	if err != nil {
		return KaminoLegRequest{}, err
	}
	deposit, borrow, repay, withdraw := kaminoMetasForRoute(route)
	metas := map[kaminoLeg][]accountMeta{kaminoLegDeposit: deposit, kaminoLegBorrow: borrow, kaminoLegRepay: repay, kaminoLegWithdraw: withdraw}[leg]
	accounts := make(KaminoLegAccounts, len(metas))
	for i, item := range metas {
		accounts[i].Address, accounts[i].Signer, accounts[i].Writable = encodeBase58(item.key[:]), item.signer, item.writable
	}
	data := binary.LittleEndian.AppendUint64(append([]byte(nil), discriminator...), amount)
	request := KaminoLegRequest{Action: action, AmountRaw: amount, Policy: policy, Accounts: accounts, Data: data,
		RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: lane}
	if _, observedLeg, err := kaminoRouteInstruction(m.vault, request, lane); err != nil || observedLeg != leg {
		return KaminoLegRequest{}, errKaminoPacketUnavailable
	}
	return request, nil
}

func (m RouteManifest) activeRuntimeRoute() (RuntimeRoute, error) {
	return m.route(m.RuntimeActivation.SelectedLane)
}
