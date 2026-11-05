package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const miniAppLink = "https://t.me/askloyal_tgbot/app"

func jsFalsy(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case bool:
		return !v
	case float64:
		return v == 0
	case string:
		return v == ""
	}
	return false
}

func jsNumber(f float64) string {
	if f == 0 {
		return "0"
	}
	if abs := math.Abs(f); abs >= 1e21 || abs < 1e-6 {
		mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
		return mantissa + "e" + exponent[:1] + strings.TrimLeft(exponent[1:], "0")
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func jsString(v any) (string, bool) {
	switch v := v.(type) {
	case string:
		return v, true
	case float64:
		return jsNumber(v), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

func formEscape(s string) string {
	var out strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '*', c == '-', c == '.', c == '_':
			out.WriteByte(c)
		case c == ' ':
			out.WriteByte('+')
		default:
			out.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)+0x100, 16)[1:]))
		}
	}
	return out.String()
}

func (b *Bot) shareImageURL(r *http.Request, sender, receiver, solAmount, usdAmount string, secure bool) string {
	base := b.publicBase
	if base == nil {
		scheme := "https"
		if proto := r.Header.Get("X-Forwarded-Proto"); proto == "http" {
			scheme = proto
		}
		base = &url.URL{Scheme: scheme, Host: r.Host}
	}
	query := [][2]string{{"sender", sender}, {"receiver", receiver}, {"solAmount", solAmount}, {"usdAmount", usdAmount}}
	if secure {
		query = append(query, [2]string{"secure", "1"})
	}
	parts := make([]string, len(query))
	for i, pair := range query {
		parts[i] = formEscape(pair[0]) + "=" + formEscape(pair[1])
	}
	return base.Scheme + "://" + base.Host + "/api/og/share?" + strings.Join(parts, "&")
}

func (b *Bot) share(w http.ResponseWriter, r *http.Request) {
	const failed = "Failed to share"
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusInternalServerError, failed)
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "initData bytes are required")
		return
	}
	var parsed any
	if err := json.Unmarshal(body, &parsed); err != nil || parsed == nil {
		writeError(w, http.StatusInternalServerError, failed)
		return
	}
	fields, _ := parsed.(map[string]any)
	rawInitData, receiver, solAmount, usdAmount, secure := fields["rawInitData"], fields["receiverUsername"], fields["solAmount"], fields["usdAmount"], fields["isSecure"]
	if jsFalsy(rawInitData) || jsFalsy(receiver) || jsFalsy(solAmount) || jsFalsy(usdAmount) {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	receiverText, okReceiver := jsString(receiver)
	solText, okSol := jsString(solAmount)
	usdText, okUSD := jsString(usdAmount)
	if !okReceiver || !okSol || !okUSD {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	rawText, ok := rawInitData.(string)
	var data *initData
	if ok {
		data, err = newInitData(b.botID, rawText)
	}
	if !ok || err != nil {
		writeError(w, http.StatusBadRequest, "Invalid initData payload")
		return
	}
	if !data.verified() {
		writeError(w, http.StatusUnauthorized, "Invalid Telegram init data")
		return
	}
	userID, sender, err := shareSender(data)
	if err != nil {
		slog.Warn("telegram share: unusable user in initData", "error", err)
		writeError(w, http.StatusInternalServerError, failed)
		return
	}
	isSecure := !jsFalsy(secure)
	photoURL := b.shareImageURL(r, sender, receiverText, solText, usdText, isSecure)

	ctx, cancel := context.WithTimeout(r.Context(), sideEffectTimeout)
	defer cancel()
	msgID, err := b.api.SavePreparedInlineMessage(ctx, userID, shareInlineResult(photoURL, sender, receiverText, solText, isSecure))
	if err != nil {
		slog.Error("telegram share: savePreparedInlineMessage failed", "error", err)
		writeError(w, http.StatusInternalServerError, failed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"msgId": msgID})
}

func shareSender(data *initData) (int64, string, error) {
	rawUser, _ := data.field("user")
	var user struct {
		ID       float64 `json:"id"`
		Username any     `json:"username"`
	}
	if err := json.Unmarshal([]byte(rawUser), &user); err != nil {
		return 0, "", err
	}
	sender := "@unknown"
	if name, ok := user.Username.(string); ok && name != "" {
		sender = name
	}
	return int64(user.ID), sender, nil
}

func shareInlineResult(photoURL, sender, receiver, solAmount string, secure bool) map[string]any {
	label := "SOL"
	if secure {
		label = "Secure SOL"
	}
	text := "Hi " + receiver + "!\n\nYou received <b>" + solAmount + " " + label + "</b> from <b>" + sender +
		"</b>.\n\nTap the button below the image to get your money!<a href=\"" + photoURL + "\">​</a>"
	button := func(text string) []map[string]string {
		return []map[string]string{{"text": text, "url": miniAppLink}}
	}
	return map[string]any{
		"type":  "article",
		"id":    "id0",
		"title": "Tap to open Loyal app and claim the transaction!",
		"reply_markup": map[string]any{"inline_keyboard": [][]map[string]string{
			button("⬇️⬇️⬇️"), button("CLICK HERE TO CLAIM"), button("⬆️⬆️⬆️"),
		}},
		"url":                   photoURL,
		"thumbnail_url":         photoURL,
		"input_message_content": map[string]any{"message_text": text, "parse_mode": "HTML"},
	}
}
