package telegram

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var telegramPublicKeys = func() []ed25519.PublicKey {
	var keys []ed25519.PublicKey
	for _, h := range []string{
		"40055058a4ee38156a06562e52eece92a771bcd8346a8c4615cb7376eddf72ec",
		"e7bf03a2fa4602af4580703d88dda5bb59f32ed8b02a56c187fe7d34caed242d",
	} {
		key, err := hex.DecodeString(h)
		if err != nil || len(key) != ed25519.PublicKeySize {
			panic("telegram mini-app public key")
		}
		keys = append(keys, key)
	}
	return keys
}()

var hexSignature = regexp.MustCompile(`^[0-9a-fA-F]+$`)

type initDataField struct {
	key    string
	values []string
}

func (f initDataField) text() string {
	if len(f.values) == 1 {
		return f.values[0]
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(f.values)
	return strings.TrimSuffix(out.String(), "\n")
}

func queryUnescape(s string) string {
	s = strings.ReplaceAll(s, "+", " ")
	if decoded, err := url.PathUnescape(s); err == nil {
		return decoded
	}
	return s
}

func parseInitData(raw string) []initDataField {
	byKey := map[string]*initDataField{}
	for _, part := range strings.Split(raw, "&") {
		name, value, _ := strings.Cut(part, "=")
		name = queryUnescape(name)
		if name == "" {
			continue
		}
		field := byKey[name]
		if field == nil {
			field = &initDataField{key: name}
			byKey[name] = field
		}
		field.values = append(field.values, queryUnescape(value))
	}
	fields := make([]initDataField, 0, len(byKey))
	for _, field := range byKey {
		fields = append(fields, *field)
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
	return fields
}

func parseSignature(signature string) ([]byte, error) {
	trimmed := strings.TrimSpace(signature)
	if hexSignature.MatchString(trimmed) && len(trimmed)%2 == 0 {
		return hex.DecodeString(trimmed)
	}
	normalized := strings.NewReplacer("-", "+", "_", "/").Replace(trimmed)
	normalized = strings.Join(strings.Fields(normalized), "")
	if rem := len(normalized) % 4; rem != 0 {
		normalized += strings.Repeat("=", 4-rem)
	}
	return base64.StdEncoding.DecodeString(normalized)
}

type initData struct {
	fields    []initDataField
	signature []byte
	message   []byte
}

var errInitDataSignature = errors.New("initData has no single signature")

func newInitData(botID int64, raw string) (*initData, error) {
	data := &initData{fields: parseInitData(raw)}
	lines := []string{strconv.FormatInt(botID, 10) + ":WebAppData"}
	var signature *initDataField
	for i, field := range data.fields {
		switch field.key {
		case "signature":
			signature = &data.fields[i]
		case "hash":
		default:
			lines = append(lines, field.key+"="+field.text())
		}
	}
	data.message = []byte(strings.Join(lines, "\n"))
	if signature == nil || len(signature.values) != 1 {
		return nil, errInitDataSignature
	}
	var err error
	if data.signature, err = parseSignature(signature.values[0]); err != nil {
		return nil, err
	}
	return data, nil
}

func (d *initData) verified() bool {
	if len(d.message) == 0 || len(d.signature) != ed25519.SignatureSize {
		return false
	}
	for _, key := range telegramPublicKeys {
		if ed25519.Verify(key, d.message, d.signature) {
			return true
		}
	}
	return false
}

func (d *initData) field(name string) (string, bool) {
	for _, field := range d.fields {
		if field.key == name && len(field.values) == 1 {
			return field.values[0], true
		}
	}
	return "", false
}
