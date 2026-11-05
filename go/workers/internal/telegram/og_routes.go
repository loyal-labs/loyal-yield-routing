package telegram

import (
	"log/slog"
	"math/rand/v2"
	"net/http"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/telegram/og"
)

func writeOG(w http.ResponseWriter, image string, png []byte, err error) {
	if err != nil {
		slog.Error("og image generation failed", "image", image, "error", err)
		http.Error(w, "Failed to generate the image", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, immutable, no-transform, max-age=31536000")
	_, _ = w.Write(png)
}

func ogShareSummary(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	png, err := og.ShareSummary(q.Get("text"), q.Get("date"), og.Theme(rand.IntN(3)), 1+rand.IntN(og.DogVariants))
	writeOG(w, "share-summary", png, err)
}

func ogShare(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	or := func(v, fallback string) string {
		if v == "" {
			return fallback
		}
		return v
	}
	png, err := og.Share(
		og.TruncateAddress(or(q.Get("sender"), "@username")),
		og.TruncateAddress(or(q.Get("receiver"), "2fKB…Nhtj")),
		og.FormatSol(q.Get("solAmount"), q.Get("secure") == "1"),
		og.FormatUsd(q.Get("usdAmount")),
	)
	writeOG(w, "share", png, err)
}
