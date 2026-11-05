package og

import (
	"image"
	"image/color"
	"math"
	"strconv"
	"strings"

	"golang.org/x/image/vector"
)

var (
	black     = color.NRGBA{0, 0, 0, 0xFF}
	halfBlack = color.NRGBA{0, 0, 0, 0x80}
	white     = color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
	loyalRed  = color.NRGBA{0xFF, 0x33, 0x47, 0xFF}
)

func jsNumber(value string) (float64, bool) {
	trimmed := strings.TrimFunc(value, func(r rune) bool { return jsWhitespace.MatchString(string(r)) })
	if trimmed == "" {
		return 0, true
	}
	lower := strings.ToLower(strings.TrimLeft(trimmed, "+-"))
	if strings.HasPrefix(lower, "inf") || strings.HasPrefix(lower, "nan") || strings.Contains(lower, "_") || strings.HasPrefix(lower, "0x") {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	return parsed, err == nil
}

func groupThousands(value float64, decimals int) string {
	formatted := strconv.FormatFloat(math.Abs(value), 'f', decimals, 64)
	whole, fraction, _ := strings.Cut(formatted, ".")
	var out strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	sign := ""
	if math.Signbit(value) {
		sign = "-"
	}
	return sign + out.String() + "." + fraction
}

func FormatSol(value string, secure bool) string {
	if value == "" {
		return "+15.0988 SOL"
	}
	label := "SOL"
	if secure {
		label = "Secure SOL"
	}
	parsed, ok := jsNumber(value)
	if !ok {
		return value + " " + label
	}
	return "+" + groupThousands(parsed, 4) + " " + label
}

func FormatUsd(value string) string {
	if value == "" {
		return "≈$2,869.77"
	}
	parsed, ok := jsNumber(value)
	if !ok {
		return "≈$" + value
	}
	return "≈$" + groupThousands(parsed, 2)
}

func TruncateAddress(value string) string {
	const maxLength, side = 12, 5
	if jsLength(value) <= maxLength {
		return value
	}
	n := jsLength(value)
	return jsSlice(value, 0, side) + "…" + jsSlice(value, n-side, n)
}

func Share(sender, receiver, solText, usdText string) ([]byte, error) {
	dst := image.NewRGBA(image.Rect(0, 0, Width, Height))
	if err := drawScaled(dst, "share-bg.png", dst.Bounds()); err != nil {
		return nil, err
	}
	styles := []textStyle{
		{weight: medium, size: 47, letterSpacing: -0.47, color: black},
		{weight: medium, size: 57, letterSpacing: -0.57, color: black},
		{weight: semiBold, size: 86, letterSpacing: -0.86, color: black},
		{weight: regular, size: 40, color: halfBlack},
		{weight: medium, size: 32, letterSpacing: -0.32, color: halfBlack},
		{weight: regular, size: 44, color: black},
	}
	faces := make([]*face, len(styles))
	for i, style := range styles {
		var err error
		if faces[i], err = newFace(style); err != nil {
			return nil, err
		}
	}
	received, loyal, sol, usd, label, address := faces[0], faces[1], faces[2], faces[3], faces[4], faces[5]

	const pad, row = 56.0, 72.0
	fill(dst, loyalRed, func(z *vector.Rasterizer) { circle(z, pad+row/2, pad+row/2, row/2) })
	scale := 40.0 / 47.0
	at := func(x, y float64) [2]float64 { return [2]float64{pad + 16 + x*scale, pad + 16 + y*scale} }
	stroke := 4.69 * scale
	roundStroke(dst, white, stroke, at(23.4424, 39.0708), at(23.4424, 7.81372))
	roundStroke(dst, white, stroke, at(35.0713, 27.4424), at(23.4427, 39.0711), at(11.814, 27.4424))
	received.draw(dst, pad+row+20, pad+(row-47)/2, "You received")
	loyal.draw(dst, Width-pad-loyal.width("Loyal"), pad+(row-57)/2, "Loyal")

	top := float64(Height) - 60 - (86 + 24 + 40 + 44 + (32 + 12 + 44) + 28 + (32 + 12 + 44))
	sol.draw(dst, pad, top, solText)
	top += 86 + 24
	usd.draw(dst, pad, top, usdText)
	top += 40 + 44
	for i, pair := range [][2]string{{"From", sender}, {"To", receiver}} {
		if i > 0 {
			top += 28
		}
		label.draw(dst, pad, top, pair[0])
		top += 32 + 12
		address.draw(dst, pad, top, pair[1])
		top += 44
	}
	return encode(dst)
}
