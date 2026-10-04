package watch

import (
	"context"
	"fmt"

	"github.com/gagliardetto/solana-go"
)

// Apps migrations 0015/0016 use mainnet, while RPC/Yield configuration uses
// mainnet-beta. Keep that mapping at the database boundary, not in durable
// channel or replay identity. Other environments have the same spelling.
func appEnvironment(cluster string) (string, error) {
	switch cluster {
	case "mainnet", "mainnet-beta":
		return "mainnet", nil
	case "testnet", "devnet", "localnet":
		return cluster, nil
	default:
		return "", fmt.Errorf("unsupported watch cluster %q", cluster)
	}
}

func (l *Loader) loadAppTargets(ctx context.Context) ([]earnTarget, []string, bool, error) {
	if l.apps == nil {
		return nil, nil, false, fmt.Errorf("watch coverage requires configured Apps database")
	}
	var users, smartAccounts bool
	if err := l.apps.QueryRow(ctx, `SELECT to_regclass('public.app_users') IS NOT NULL,to_regclass('public.app_user_smart_accounts') IS NOT NULL`).Scan(&users, &smartAccounts); err != nil {
		return nil, nil, false, fmt.Errorf("read Apps watch schema: %w", err)
	}
	if !users || !smartAccounts {
		if l.requireApps || users != smartAccounts {
			return nil, nil, false, fmt.Errorf("watch coverage requires complete Apps users/smart-accounts schema")
		}
		return nil, nil, false, nil
	}
	environment, err := appEnvironment(l.cluster)
	if err != nil {
		return nil, nil, false, err
	}
	rows, err := l.apps.Query(ctx, `SELECT smart.settings_pda,app.subject_address,smart.state
 FROM public.app_user_smart_accounts smart JOIN public.app_users app ON app.id=smart.user_id
 WHERE smart.solana_env=$1 ORDER BY smart.settings_pda`, environment)
	if err != nil {
		return nil, nil, false, fmt.Errorf("load Apps watch identities: %w", err)
	}
	defer rows.Close()
	var targets []earnTarget
	settings := []string{}
	for rows.Next() {
		var setting, wallet, state string
		if err := rows.Scan(&setting, &wallet, &state); err != nil {
			return nil, nil, false, err
		}
		if _, err := solana.PublicKeyFromBase58(setting); err != nil {
			return nil, nil, false, fmt.Errorf("Apps watch settings identity is invalid: %w", err)
		}
		// Provisioning/failed settings still bound durable Yield recovery in
		// the retained loader. Only ready Apps rows create a base vault watch.
		settings = append(settings, setting)
		switch state {
		case "ready":
			targets = append(targets, earnTarget{Environment: l.cluster, Settings: setting, Wallet: wallet, VaultIndex: 1})
		case "provisioning", "failed":
		default:
			return nil, nil, false, fmt.Errorf("Apps watch identity has unsupported state %q", state)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, false, err
	}
	return targets, settings, true, nil
}
