package webapp

import (
	"bytes"
	"crypto/ed25519"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mr-tron/base58"
)

type siwsOutput struct {
	address       string
	publicKey     []byte
	signedMessage []byte
	signature     []byte
}

var siwsMessagePattern = regexp.MustCompile(`^(?P<domain>[^\n]+?) wants you to sign in with your Solana account:\n` +
	`(?P<address>[^\n]+)(?:\n|$)` +
	`(?:\n(?P<statement>[\S\s]*?)(?:\n|$))??` +
	`(?:\nURI: (?P<uri>[^\n]+))?` +
	`(?:\nVersion: (?P<version>[^\n]+))?` +
	`(?:\nChain ID: (?P<chainId>[^\n]+))?` +
	`(?:\nNonce: (?P<nonce>[^\n]+))?` +
	`(?:\nIssued At: (?P<issuedAt>[^\n]+))?` +
	`(?:\nExpiration Time: (?P<expirationTime>[^\n]+))?` +
	`(?:\nNot Before: (?P<notBefore>[^\n]+))?` +
	`(?:\nRequest ID: (?P<requestId>[^\n]+))?` +
	`(?:\nResources:(?P<resources>(?:\n- [^\n]+)*))?` +
	`\n*$`)

func parseByteArray(value any) ([]byte, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	out := make([]byte, len(items))
	for i, item := range items {
		number, ok := item.(float64)
		if !ok || number < 0 || number > 255 || number != math.Trunc(number) {
			return nil, false
		}
		out[i] = byte(number)
	}
	return out, true
}

func optionalNonEmpty(object map[string]any, key string) bool {
	value, present := object[key]
	if !present {
		return true
	}
	_, ok := nonEmptyString(value)
	return ok
}

func nonEmptyStrings(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if _, ok := nonEmptyString(item); !ok {
			return false
		}
	}
	return true
}

func parseSiwsOutput(value any) (siwsOutput, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return siwsOutput{}, false
	}
	account, ok := object["account"].(map[string]any)
	if !ok {
		return siwsOutput{}, false
	}
	address, hasAddress := nonEmptyString(account["address"])
	publicKey, hasPublicKey := parseByteArray(account["publicKey"])
	if !hasAddress || !hasPublicKey || !nonEmptyStrings(account["features"]) || !nonEmptyStrings(account["chains"]) ||
		!optionalNonEmpty(account, "label") || !optionalNonEmpty(account, "icon") {
		return siwsOutput{}, false
	}
	signedMessage, hasMessage := parseByteArray(object["signedMessage"])
	signature, hasSignature := parseByteArray(object["signature"])
	if !hasMessage || !hasSignature {
		return siwsOutput{}, false
	}
	if signatureType, present := object["signatureType"]; present && signatureType != "ed25519" {
		return siwsOutput{}, false
	}
	return siwsOutput{address: address, publicKey: publicKey, signedMessage: signedMessage, signature: signature}, true
}

func parseSiwsInput(value any) (*WalletSignInInput, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	input := &WalletSignInInput{}
	fields := []struct {
		key  string
		into **string
	}{
		{"domain", &input.Domain}, {"address", &input.Address}, {"statement", &input.Statement}, {"uri", &input.URI},
		{"version", &input.Version}, {"chainId", &input.ChainID}, {"nonce", &input.Nonce}, {"issuedAt", &input.IssuedAt},
		{"expirationTime", &input.ExpirationTime}, {"notBefore", &input.NotBefore}, {"requestId", &input.RequestID},
	}
	for _, field := range fields {
		raw, present := object[field.key]
		if !present {
			continue
		}
		text, ok := nonEmptyString(raw)
		if !ok {
			return nil, false
		}
		*field.into = &text
	}
	if raw, present := object["resources"]; present {
		if !nonEmptyStrings(raw) {
			return nil, false
		}
		resources := []string{}
		for _, item := range raw.([]any) {
			resources = append(resources, item.(string))
		}
		input.Resources = &resources
	}
	return input, true
}

func verifySiwsOutput(input *WalletSignInInput, output siwsOutput) (string, error) {
	trimmed := bytes.TrimLeft(output.publicKey, "\x00")
	if len(trimmed) > 32 {
		return "", walletRefusal(http.StatusBadRequest, "invalid_wallet_signin_public_key", "Wallet sign-in public key is invalid.")
	}
	padded := make([]byte, 32)
	copy(padded[32-len(trimmed):], trimmed)
	derived := base58.Encode(padded)
	if derived != output.address {
		return "", walletRefusal(http.StatusUnauthorized, "wallet_signin_address_mismatch", "Wallet sign-in account address does not match the public key.")
	}
	if !verifySignIn(input, output) {
		refusal := walletRefusal(http.StatusUnauthorized, "invalid_wallet_signin", "Wallet sign-in could not be verified.")
		refusal.Detail.Details = walletAddressDetails{WalletAddress: derived}
		return "", refusal
	}
	return derived, nil
}

func verifySignIn(input *WalletSignInInput, output siwsOutput) bool {
	if !utf8.Valid(output.signedMessage) {
		return false
	}
	text := string(output.signedMessage)
	match := siwsMessagePattern.FindStringSubmatchIndex(text)
	if match == nil {
		return false
	}
	group := func(name string) *string {
		i := siwsMessagePattern.SubexpIndex(name)
		if match[2*i] < 0 {
			return nil
		}
		value := text[match[2*i]:match[2*i+1]]
		return &value
	}
	parsed := WalletSignInInput{
		Domain: group("domain"), Address: group("address"), Statement: group("statement"), URI: group("uri"),
		Version: group("version"), ChainID: group("chainId"), Nonce: group("nonce"), IssuedAt: group("issuedAt"),
		ExpirationTime: group("expirationTime"), NotBefore: group("notBefore"), RequestID: group("requestId"),
	}
	if raw := group("resources"); raw != nil {
		resources := strings.Split(*raw, "\n- ")[1:]
		parsed.Resources = &resources
	}
	same := func(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	if input.Domain != nil && *input.Domain != "" && !same(input.Domain, parsed.Domain) {
		return false
	}
	if input.Address != nil && *input.Address != "" && !same(input.Address, parsed.Address) {
		return false
	}
	if !same(input.Statement, parsed.Statement) || !same(input.URI, parsed.URI) || !same(input.Version, parsed.Version) ||
		!same(input.ChainID, parsed.ChainID) || !same(input.Nonce, parsed.Nonce) || !same(input.IssuedAt, parsed.IssuedAt) ||
		!same(input.ExpirationTime, parsed.ExpirationTime) || !same(input.NotBefore, parsed.NotBefore) ||
		!same(input.RequestID, parsed.RequestID) {
		return false
	}
	if (input.Resources == nil) != (parsed.Resources == nil) ||
		input.Resources != nil && !slices.Equal(*input.Resources, *parsed.Resources) {
		return false
	}
	if !bytes.Equal([]byte(siwsMessageText(parsed)), output.signedMessage) {
		return false
	}
	return len(output.publicKey) == ed25519.PublicKeySize && ed25519.Verify(output.publicKey, output.signedMessage, output.signature)
}

func siwsMessageText(input WalletSignInInput) string {
	text := func(value *string) string {
		if value == nil {
			return ""
		}
		return *value
	}
	message := text(input.Domain) + " wants you to sign in with your Solana account:\n" + text(input.Address)
	if text(input.Statement) != "" {
		message += "\n\n" + *input.Statement
	}
	var fields []string
	for _, field := range []struct{ label, value string }{
		{"URI: ", text(input.URI)}, {"Version: ", text(input.Version)}, {"Chain ID: ", text(input.ChainID)},
		{"Nonce: ", text(input.Nonce)}, {"Issued At: ", text(input.IssuedAt)}, {"Expiration Time: ", text(input.ExpirationTime)},
		{"Not Before: ", text(input.NotBefore)}, {"Request ID: ", text(input.RequestID)},
	} {
		if field.value != "" {
			fields = append(fields, field.label+field.value)
		}
	}
	if input.Resources != nil {
		fields = append(fields, "Resources:")
		for _, resource := range *input.Resources {
			fields = append(fields, "- "+resource)
		}
	}
	if len(fields) > 0 {
		message += "\n\n" + strings.Join(fields, "\n")
	}
	return message
}
