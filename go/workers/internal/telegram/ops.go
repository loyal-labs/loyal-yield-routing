package telegram

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/r2"
)

const (
	libraryUploadMaxBytes = 5 << 20
	libraryUploadTimeout  = 60 * time.Second
	proxyImageMaxBytes    = 16 << 20
	proxyImageTimeout     = 20 * time.Second
)

var (
	proxyImageHosts  = []string{"t.me", "cdn1.telesco.pe", "cdn2.telesco.pe", "cdn3.telesco.pe", "cdn4.telesco.pe", "cdn5.telesco.pe"}
	libraryTypes     = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}
	proxyImageClient = &http.Client{
		Timeout:       proxyImageTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
)

func writeJSON(w http.ResponseWriter, status int, body any) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes.TrimSuffix(out.Bytes(), []byte("\n")))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (b *Bot) registerOps(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/telegram/proxy-image", b.proxyImage)
	mux.HandleFunc("POST /api/telegram/share", b.share)
	mux.HandleFunc("POST /api/admin/library/upload", b.libraryUpload)
}

func proxyImageAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, allowed := range proxyImageHosts {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

func (b *Bot) proxyImage(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		writeError(w, http.StatusBadRequest, "Missing url parameter")
		return
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme == "" || target.User != nil || ((target.Scheme == "http" || target.Scheme == "https") && target.Host == "") {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	if target.Scheme != "https" || !proxyImageAllowed(target.Hostname()) {
		writeError(w, http.StatusBadRequest, "Invalid host")
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	response, err := proxyImageClient.Do(request)
	if err != nil {
		slog.Warn("proxy-image fetch failed", "host", target.Hostname())
		writeError(w, http.StatusInternalServerError, "Internal server error")
		return
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		writeError(w, http.StatusBadGateway, "Redirects are not followed")
		return
	case response.StatusCode < 200 || response.StatusCode > 299:
		writeError(w, response.StatusCode, "Failed to fetch image: "+strings.TrimPrefix(response.Status, fmt.Sprint(response.StatusCode)+" "))
		return
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") || strings.HasPrefix(contentType, "image/svg+xml") {
		writeError(w, http.StatusUnsupportedMediaType, "Upstream is not an image")
		return
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, proxyImageMaxBytes+1))
	if err != nil || len(body) > proxyImageMaxBytes {
		writeError(w, http.StatusBadGateway, "Upstream image too large")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(body)
}

func (b *Bot) libraryUploadAuthorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return presented != "" && subtle.ConstantTimeCompare([]byte(presented), b.uploadToken) == 1
}

func (b *Bot) libraryUpload(w http.ResponseWriter, r *http.Request) {
	if b.r2 == nil || len(b.uploadToken) == 0 {
		writeError(w, http.StatusServiceUnavailable, "Library upload is not configured on this host")
		return
	}
	if !b.libraryUploadAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	part, status, message := firstFilePart(w, r)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	extension, ok := libraryTypes[part.contentType]
	if !ok {
		writeError(w, http.StatusBadRequest, "Unsupported content type; allowed: image/png, image/jpeg, image/webp")
		return
	}
	body, err := io.ReadAll(io.LimitReader(part.reader, libraryUploadMaxBytes+1))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "File exceeds 5MB limit")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "Expected multipart/form-data body")
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "Uploaded file is empty")
		return
	}
	if len(body) > libraryUploadMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "File exceeds 5MB limit")
		return
	}
	id, err := r2.NewObjectID()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	key := b.r2.Key("library/" + id + "." + extension)
	ctx, cancel := context.WithTimeout(r.Context(), libraryUploadTimeout)
	defer cancel()
	if err := b.r2.Put(ctx, key, body, part.contentType); err != nil {
		slog.Error("library upload to R2 failed", "error", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": b.r2.PublicURL(key)})
}

type filePart struct {
	contentType string
	reader      io.Reader
}

func firstFilePart(w http.ResponseWriter, r *http.Request) (filePart, int, string) {
	const notMultipart = "Expected multipart/form-data body"
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType == "application/x-www-form-urlencoded" {
		return filePart{}, http.StatusBadRequest, "`file` field is required"
	}
	r.Body = http.MaxBytesReader(w, r.Body, libraryUploadMaxBytes+(1<<20))
	reader, err := r.MultipartReader()
	if err != nil {
		return filePart{}, http.StatusBadRequest, notMultipart
	}
	for {
		part, err := reader.NextPart()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return filePart{}, http.StatusBadRequest, "`file` field is required"
			}
			return filePart{}, http.StatusBadRequest, notMultipart
		}
		if part.FormName() != "file" {
			continue
		}
		if part.FileName() == "" {
			return filePart{}, http.StatusBadRequest, "`file` field is required"
		}
		return filePart{contentType: strings.ToLower(part.Header.Get("Content-Type")), reader: part}, 0, ""
	}
}
