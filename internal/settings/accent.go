package settings

import (
	"encoding/hex"
	"fmt"
	"math"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/problem"
)

type rgb [3]uint8

type accentScale struct {
	S300 string `json:"300"`
	S400 string `json:"400"`
	S500 string `json:"500"`
	S600 string `json:"600"`
	S700 string `json:"700"`
}

var (
	white        = rgb{0xff, 0xff, 0xff}
	black        = rgb{}
	consoleVoid  = rgb{0x05, 0x06, 0x08}
	consoleHover = rgb{0x17, 0x1b, 0x24}
	consolePaper = rgb{0xec, 0xef, 0xf4}
)

func normalizeAccent(raw string) (string, error) {
	accent := strings.ToLower(strings.TrimSpace(raw))
	if accent == "" {
		return "", nil
	}
	_, err := deriveAccent(accent)
	return accent, err
}

func deriveAccent(accent string) (accentScale, error) {
	base, ok := parseRGB(accent)
	if !ok {
		return accentScale{}, problem.Invalid("accent", "Use a brand color in #rrggbb form.")
	}
	if contrast(white, base) < 4.5 {
		return accentScale{}, problem.Invalid("accent", "Choose a darker brand color. White button text needs 4.5:1 contrast.")
	}
	return accentScale{
		S300: firstPassing(base, white, 0, consoleVoid, 7).String(),
		S400: firstPassing(base, white, 0, consoleHover, 4.5).String(),
		S500: base.String(),
		S600: mix(base, black, 15).String(),
		S700: firstPassing(base, black, 30, consolePaper, 4.5).String(),
	}, nil
}

func parseRGB(value string) (rgb, bool) {
	if len(value) != 7 || value[0] != '#' {
		return rgb{}, false
	}
	decoded, err := hex.DecodeString(value[1:])
	if err != nil {
		return rgb{}, false
	}
	return rgb{decoded[0], decoded[1], decoded[2]}, true
}

func (c rgb) String() string { return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]) }

func firstPassing(base, toward rgb, fromPercent int, surface rgb, minimum float64) rgb {
	for percent := fromPercent; percent < 100; percent++ {
		if candidate := mix(base, toward, percent); contrast(candidate, surface) >= minimum {
			return candidate
		}
	}
	return toward
}

func mix(base, toward rgb, percent int) rgb {
	channel := func(i int) uint8 {
		return uint8(math.Round((float64(base[i])*float64(100-percent) + float64(toward[i])*float64(percent)) / 100))
	}
	return rgb{channel(0), channel(1), channel(2)}
}

func luminance(c rgb) float64 {
	linear := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(c[0]) + 0.7152*linear(c[1]) + 0.0722*linear(c[2])
}

func contrast(a, b rgb) float64 {
	la, lb := luminance(a), luminance(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}
