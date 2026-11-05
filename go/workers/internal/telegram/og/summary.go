package og

import (
	"fmt"
	"image"
	"image/color"
	"regexp"
	"strings"
)

type Theme int

const (
	ThemeBlack Theme = iota
	ThemeWhite
	ThemeRed
	themeCount
)

const DogVariants = 21

type themeConfig struct {
	background string
	dogPrefix  string
	text       color.NRGBA
	dateAlpha  float64
}

var themes = [themeCount]themeConfig{
	ThemeBlack: {"bg_black.png", "Dark", color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}, 0.6},
	ThemeWhite: {"bg_white.png", "Red", color.NRGBA{0, 0, 0, 0xFF}, 0.6},
	ThemeRed:   {"bg_red.png", "Red", color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}, 0.8},
}

const (
	summaryMaxText    = 200
	summaryPad        = 56
	summaryTextWidth  = 750
	summaryWordMargin = 16
	summaryDogSize    = 450
)

var jsWhitespace = regexp.MustCompile(`[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+`)

func truncate(text string, limit int) string {
	if jsLength(text) <= limit {
		return text
	}
	return strings.TrimRightFunc(jsSlice(text, 0, limit-3), func(r rune) bool { return jsWhitespace.MatchString(string(r)) }) + "..."
}

func withAlpha(c color.NRGBA, alpha float64) color.NRGBA {
	c.A = uint8(alpha*255 + 0.5)
	return c
}

func ShareSummary(text, date string, theme Theme, dog int) ([]byte, error) {
	if theme < 0 || theme >= themeCount || dog < 1 || dog > DogVariants {
		return nil, fmt.Errorf("theme %d dog %d out of range", theme, dog)
	}
	config := themes[theme]
	words := jsWhitespace.Split(strings.ReplaceAll(truncate(text, summaryMaxText), "{img}", " "), -1)
	wordFace, err := newFace(textStyle{weight: medium, size: 56, lineHeight: 1.2, letterSpacing: -1.12, color: config.text})
	if err != nil {
		return nil, err
	}
	labelFace, err := newFace(textStyle{weight: regular, size: 30, lineHeight: 1, color: withAlpha(config.text, config.dateAlpha)})
	if err != nil {
		return nil, err
	}
	dst := image.NewRGBA(image.Rect(0, 0, Width, Height))
	if err := drawScaled(dst, "bgs/"+config.background, dst.Bounds()); err != nil {
		return nil, err
	}

	right := float64(Width - summaryPad)
	wordFace.draw(dst, right-wordFace.width(date), summaryPad, date)
	labelFace.draw(dst, right-labelFace.width("Summary"), summaryPad+wordFace.lineBox()+2, "Summary")

	x, line := 0.0, 0
	for _, word := range words {
		if word == "" {
			continue
		}
		outer := wordFace.width(word) + summaryWordMargin
		if x > 0 && x+outer > summaryTextWidth {
			x, line = 0, line+1
		}
		wordFace.draw(dst, summaryPad+x, summaryPad+float64(line)*wordFace.lineBox(), word)
		x += outer
	}

	dogRect := image.Rect(Width-summaryDogSize, Height-summaryDogSize, Width, Height)
	if err := drawScaled(dst, fmt.Sprintf("dogs/%s%02d.png", config.dogPrefix, dog), dogRect); err != nil {
		return nil, err
	}
	return encode(dst)
}
