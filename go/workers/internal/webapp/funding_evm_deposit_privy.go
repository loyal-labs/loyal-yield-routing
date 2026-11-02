package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

var privyAPIBase = "https://api.privy.io/v1"

type privyRefused struct {
	status int
	body   string
}

func (e *privyRefused) Error() string {
	return fmt.Sprintf("privy refused with status %d: %s", e.status, e.body)
}

type privyDepositAddressRoute struct {
	DepositAddress string `json:"deposit_address"`
	WalletID       string `json:"wallet_id"`
}

func (h earnRoutes) privyCall(ctx context.Context, method, target string, body []byte, headers map[string]string, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(h.Privy.AppID, h.Privy.AppSecret)
	req.Header.Set("privy-app-id", h.Privy.AppID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	res, err := h.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("privy %s %s: %w", method, req.URL.Path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 400 && res.StatusCode < 500 {
		return &privyRefused{status: res.StatusCode, body: string(raw)}
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("privy %s %s: status %d", method, req.URL.Path, res.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("privy %s %s: parse response: %w", method, req.URL.Path, err)
	}
	return nil
}

func (h earnRoutes) privySolanaWalletID(ctx context.Context, address string) (string, error) {
	var page struct {
		Data []struct {
			ID        string `json:"id"`
			Address   string `json:"address"`
			ChainType string `json:"chain_type"`
		} `json:"data"`
	}
	query := url.Values{"address": {address}, "chain_type": {"solana"}}
	if err := h.privyCall(ctx, http.MethodGet, privyAPIBase+"/wallets?"+query.Encode(), nil, nil, &page); err != nil {
		return "", err
	}
	for _, wallet := range page.Data {
		if wallet.ChainType == "solana" && wallet.Address == address && wallet.ID != "" {
			return wallet.ID, nil
		}
	}
	return "", nil
}

func privyDepositRoutesURL(walletID string) string {
	return privyAPIBase + "/wallets/" + url.PathEscape(walletID) + "/deposit_accounts/crypto"
}

func (h earnRoutes) privyDepositRoutes(ctx context.Context, walletID string) ([]privyDepositAddressRoute, error) {
	var page struct {
		Data []privyDepositAddressRoute `json:"data"`
	}
	err := h.privyCall(ctx, http.MethodGet, privyDepositRoutesURL(walletID), nil, nil, &page)
	return page.Data, err
}

func (h earnRoutes) privyCreateDepositRoutes(ctx context.Context, signed PrivyDepositRequest, signature string) ([]privyDepositAddressRoute, error) {
	body, err := json.Marshal(signed.Body)
	if err != nil {
		return nil, err
	}
	var created struct {
		DepositAccounts []privyDepositAddressRoute `json:"deposit_accounts"`
	}
	err = h.privyCall(ctx, signed.Method, signed.URL, body, map[string]string{
		"privy-app-id":                  signed.Headers.AppID,
		"privy-request-expiry":          signed.Headers.RequestExpiry,
		"privy-authorization-signature": signature,
	}, &created)
	return created.DepositAccounts, err
}
