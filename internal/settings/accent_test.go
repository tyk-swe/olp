package settings

import (
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
)

func TestContrastMatchesWCAGReferenceValues(t *testing.T) {
	for _, tc := range []struct {
		a, b rgb
		want float64
	}{
		{white, black, 21},
		{rgb{0x77, 0x77, 0x77}, white, 4.48},
		{white, white, 1},
	} {
		if got := contrast(tc.a, tc.b); math.Abs(got-tc.want) > 0.01 {
			t.Errorf("contrast(%s, %s) = %.3f, want %.2f", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDerivedAccentShadesStayReadableOnConsoleSurfaces(t *testing.T) {
	const digits = "0123456789abcdef"
	accepted := 0
	for _, r := range digits {
		for _, g := range digits {
			for _, b := range digits {
				input := fmt.Sprintf("#%c%c%c%c%c%c", r, r, g, g, b, b)
				base, _ := parseRGB(input)
				scale, err := deriveAccent(input)
				if contrast(white, base) < 4.5 {
					if err == nil {
						t.Errorf("%s: accepted a color white text cannot read on", input)
					}
					continue
				}
				if err != nil {
					t.Fatalf("%s: %v", input, err)
				}
				accepted++
				shade := func(hex string) rgb {
					c, ok := parseRGB(hex)
					if !ok {
						t.Fatalf("%s: malformed shade %q", input, hex)
					}
					return c
				}
				checks := []struct {
					step    string
					fg, bg  rgb
					minimum float64
				}{
					{"300", shade(scale.S300), consoleVoid, 7},
					{"400", shade(scale.S400), consoleHover, 4.5},
					{"500", white, shade(scale.S500), 4.5},
					{"600", white, shade(scale.S600), 4.5},
					{"700", shade(scale.S700), consolePaper, 4.5},
				}
				for _, check := range checks {
					if got := contrast(check.fg, check.bg); got < check.minimum {
						t.Errorf("%s step %s: contrast %.2f below %.1f", input, check.step, got, check.minimum)
					}
				}
				if scale.S500 != input {
					t.Errorf("%s: primary shade %s is not the chosen color", input, scale.S500)
				}
				if base != black && (luminance(shade(scale.S600)) >= luminance(base) || luminance(shade(scale.S700)) >= luminance(shade(scale.S600))) {
					t.Errorf("%s: 600 %s and 700 %s are not progressively darker", input, scale.S600, scale.S700)
				}
			}
		}
	}
	if accepted == 0 {
		t.Fatal("no color was accepted")
	}
}

func TestNormalizeAccent(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		valid     bool
	}{
		{"", "", true},
		{" #2F64EE ", "#2f64ee", true},
		{"#ffff00", "", false},
		{"#fff", "", false},
		{"2f64ee", "", false},
		{"#2f64eg", "", false},
	} {
		got, err := normalizeAccent(tc.raw)
		if (err == nil) != tc.valid || (tc.valid && got != tc.want) {
			t.Errorf("normalizeAccent(%q) = %q, %v", tc.raw, got, err)
		}
	}
}

func TestAccentSurfacesMatchConsoleTheme(t *testing.T) {
	css, err := os.ReadFile("../../console/src/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for token, surface := range map[string]rgb{"void": consoleVoid, "carbon-hover": consoleHover, "paper": consolePaper} {
		if declaration := fmt.Sprintf("--color-%s: %s;", token, surface); !strings.Contains(string(css), declaration) {
			t.Errorf("app.css lacks %q, so derived accent shades may not clear contrast on that surface", declaration)
		}
	}
}
