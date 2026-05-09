package main

import (
	"strings"
	"testing"
)

// itermDraculaSample is a trimmed iTerm2 .itermcolors plist with the
// 16 ANSI entries from the canonical Dracula palette. Components are
// the 0..1 fractions iTerm writes natively.
var itermDraculaSample = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
` +
	makeITermAnsi(0, 0.156, 0.164, 0.211) +
	makeITermAnsi(1, 1.000, 0.333, 0.333) +
	makeITermAnsi(2, 0.313, 0.980, 0.482) +
	makeITermAnsi(3, 0.945, 0.980, 0.549) +
	makeITermAnsi(4, 0.741, 0.576, 0.976) +
	makeITermAnsi(5, 1.000, 0.474, 0.776) +
	makeITermAnsi(6, 0.545, 0.913, 0.992) +
	makeITermAnsi(7, 0.972, 0.972, 0.949) +
	makeITermAnsi(8, 0.266, 0.278, 0.352) +
	makeITermAnsi(9, 1.000, 0.333, 0.333) +
	makeITermAnsi(10, 0.313, 0.980, 0.482) +
	makeITermAnsi(11, 0.945, 0.980, 0.549) +
	makeITermAnsi(12, 0.741, 0.576, 0.976) +
	makeITermAnsi(13, 1.000, 0.474, 0.776) +
	makeITermAnsi(14, 0.545, 0.913, 0.992) +
	makeITermAnsi(15, 0.972, 0.972, 0.949) +
	`</dict>
</plist>`

func makeITermAnsi(idx int, r, g, b float64) string {
	return `<key>Ansi ` + itoa(idx) + ` Color</key>
<dict>
<key>Color Space</key><string>sRGB</string>
<key>Red Component</key><real>` + ftoa(r) + `</real>
<key>Green Component</key><real>` + ftoa(g) + `</real>
<key>Blue Component</key><real>` + ftoa(b) + `</real>
</dict>
`
}

func itoa(i int) string {
	if i < 10 {
		return string(rune('0' + i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func ftoa(f float64) string {
	if f >= 0.9995 {
		return "1.000"
	}
	if f < 0 {
		f = 0
	}
	whole := int(f*1000 + 0.5)
	hundreds := whole / 100
	tens := (whole / 10) % 10
	ones := whole % 10
	return "0." + string(rune('0'+hundreds)) + string(rune('0'+tens)) + string(rune('0'+ones))
}

func TestItermPaletteDracula(t *testing.T) {
	p, err := itermPalette([]byte(itermDraculaSample))
	if err != nil {
		t.Fatalf("itermPalette: %v", err)
	}
	// Index 4 (blue) -- Dracula purple #BD93F9 -> 189,147,249
	if p.ansi[4].r < 185 || p.ansi[4].r > 192 {
		t.Errorf("ansi 4 R = %d, want ~189", p.ansi[4].r)
	}
	if p.ansi[4].g < 144 || p.ansi[4].g > 150 {
		t.Errorf("ansi 4 G = %d, want ~147", p.ansi[4].g)
	}
	// Index 1 (red) -- Dracula red #FF5555 -> 255,85,85
	if p.ansi[1].r != 255 {
		t.Errorf("ansi 1 R = %d, want 255", p.ansi[1].r)
	}
}

const alacrittyCatppuccinSample = `# Catppuccin Mocha sample
[colors.primary]
background = "#1e1e2e"
foreground = "#cdd6f4"

[colors.normal]
black   = "#45475a"
red     = "#f38ba8"
green   = "#a6e3a1"
yellow  = "#f9e2af"
blue    = "#89b4fa"
magenta = "#f5c2e7"
cyan    = "#94e2d5"
white   = "#bac2de"

[colors.bright]
black   = "#585b70"
red     = "#f38ba8"
green   = "#a6e3a1"
yellow  = "#f9e2af"
blue    = "#89b4fa"
magenta = "#f5c2e7"
cyan    = "#94e2d5"
white   = "#a6adc8"
`

func TestAlacrittyTOMLPaletteCatppuccin(t *testing.T) {
	p, err := alacrittyTOMLPalette([]byte(alacrittyCatppuccinSample))
	if err != nil {
		t.Fatalf("alacrittyTOMLPalette: %v", err)
	}
	// Blue -- #89b4fa -> 137, 180, 250
	if p.ansi[4].r != 0x89 || p.ansi[4].g != 0xb4 || p.ansi[4].b != 0xfa {
		t.Errorf("normal blue = %d/%d/%d, want 137/180/250",
			p.ansi[4].r, p.ansi[4].g, p.ansi[4].b)
	}
	// Bright black -- #585b70
	if p.ansi[8].r != 0x58 || p.ansi[8].g != 0x5b || p.ansi[8].b != 0x70 {
		t.Errorf("bright black = %d/%d/%d, want 88/91/112",
			p.ansi[8].r, p.ansi[8].g, p.ansi[8].b)
	}
}

func TestThemeToLSColorsHasTruecolor(t *testing.T) {
	p, err := alacrittyTOMLPalette([]byte(alacrittyCatppuccinSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := themeToLSColorsShell(p)
	// di should use blue (137;180;250) with bold prefix
	want := "di=01;38;2;137;180;250"
	if !strings.Contains(out, want) {
		t.Errorf("missing %q in output:\n%s", want, out)
	}
	// archives should use red (243;139;168) without bold
	wantRed := "*.tar=38;2;243;139;168"
	if !strings.Contains(out, wantRed) {
		t.Errorf("missing %q in output", wantRed)
	}
}

const alacrittyYAMLSample = `# Catppuccin Mocha (legacy YAML)
colors:
  primary:
    background: '#1e1e2e'
    foreground: '#cdd6f4'
  cursor:
    text: '#1e1e2e'
    cursor: '#f5e0dc'
  normal:
    black:   '#45475a'
    red:     '#f38ba8'
    green:   '#a6e3a1'
    yellow:  '#f9e2af'
    blue:    '#89b4fa'
    magenta: '#f5c2e7'
    cyan:    '#94e2d5'
    white:   '#bac2de'
  bright:
    black:   '#585b70'
    red:     '#f38ba8'
    green:   '#a6e3a1'
    yellow:  '#f9e2af'
    blue:    '#89b4fa'
    magenta: '#f5c2e7'
    cyan:    '#94e2d5'
    white:   '#a6adc8'
  dim:
    black:   '#1e1e2e'
`

func TestAlacrittyYAMLPaletteCatppuccin(t *testing.T) {
	p, err := alacrittyYAMLPalette([]byte(alacrittyYAMLSample))
	if err != nil {
		t.Fatalf("alacrittyYAMLPalette: %v", err)
	}
	if p.ansi[4].r != 0x89 || p.ansi[4].g != 0xb4 || p.ansi[4].b != 0xfa {
		t.Errorf("normal blue = %d/%d/%d, want 137/180/250",
			p.ansi[4].r, p.ansi[4].g, p.ansi[4].b)
	}
	// `dim:` follows `bright:` in the file; the section reset should
	// have kicked in by the time we parsed `dim.black`, so bright[0]
	// must still be #585b70 (88/91/112), NOT #1e1e2e.
	if p.ansi[8].r != 0x58 || p.ansi[8].g != 0x5b || p.ansi[8].b != 0x70 {
		t.Errorf("bright black after dim leak: %d/%d/%d, want 88/91/112",
			p.ansi[8].r, p.ansi[8].g, p.ansi[8].b)
	}
}

const kittyCatppuccinSample = `# Kitty terminal: Catppuccin Mocha
foreground       #cdd6f4
background       #1e1e2e

color0  #45475a
color8  #585b70
color1  #f38ba8
color9  #f38ba8
color2  #a6e3a1
color10 #a6e3a1
color3  #f9e2af
color11 #f9e2af
color4  #89b4fa
color12 #89b4fa
color5  #f5c2e7
color13 #f5c2e7
color6  #94e2d5
color14 #94e2d5
color7  #bac2de
color15 #a6adc8
`

func TestKittyPaletteCatppuccin(t *testing.T) {
	p, err := kittyPalette([]byte(kittyCatppuccinSample))
	if err != nil {
		t.Fatalf("kittyPalette: %v", err)
	}
	if p.ansi[4].r != 0x89 || p.ansi[4].g != 0xb4 || p.ansi[4].b != 0xfa {
		t.Errorf("color4 = %d/%d/%d, want 137/180/250",
			p.ansi[4].r, p.ansi[4].g, p.ansi[4].b)
	}
	if p.ansi[15].r != 0xa6 || p.ansi[15].g != 0xad || p.ansi[15].b != 0xc8 {
		t.Errorf("color15 = %d/%d/%d, want 166/173/200",
			p.ansi[15].r, p.ansi[15].g, p.ansi[15].b)
	}
}

const xresourcesCatppuccinSample = `! Catppuccin Mocha (Xresources)
*.foreground: #cdd6f4
*.background: #1e1e2e
*.cursorColor: #f5e0dc

! Black
*.color0:  #45475a
*.color8:  #585b70

! Red
*.color1:  #f38ba8
*.color9:  #f38ba8

! Green
*.color2:  #a6e3a1
*.color10: #a6e3a1

! Yellow
*.color3:  #f9e2af
*.color11: #f9e2af

! Blue
*.color4:  #89b4fa
*.color12: #89b4fa

! Magenta
*.color5:  #f5c2e7
*.color13: #f5c2e7

! Cyan
*.color6:  #94e2d5
*.color14: #94e2d5

! White
*.color7:  #bac2de
*.color15: #a6adc8
`

func TestXresourcesPaletteCatppuccin(t *testing.T) {
	p, err := xresourcesPalette([]byte(xresourcesCatppuccinSample))
	if err != nil {
		t.Fatalf("xresourcesPalette: %v", err)
	}
	if p.ansi[4].r != 0x89 || p.ansi[4].g != 0xb4 || p.ansi[4].b != 0xfa {
		t.Errorf("color4 = %d/%d/%d, want 137/180/250",
			p.ansi[4].r, p.ansi[4].g, p.ansi[4].b)
	}
	if p.ansi[8].r != 0x58 || p.ansi[8].g != 0x5b || p.ansi[8].b != 0x70 {
		t.Errorf("color8 = %d/%d/%d, want 88/91/112",
			p.ansi[8].r, p.ansi[8].g, p.ansi[8].b)
	}
}

func TestXresourcesNoClassPrefix(t *testing.T) {
	// Some files just use bare `colorN:` without a class prefix.
	src := `color0: #112233
color1: #445566
color2: #778899
color3: #aabbcc
color4: #ddeeff
color5: #001122
color6: #334455
color7: #667788
`
	p, err := xresourcesPalette([]byte(src))
	if err != nil {
		t.Fatalf("xresourcesPalette: %v", err)
	}
	if p.ansi[4].r != 0xdd || p.ansi[4].g != 0xee || p.ansi[4].b != 0xff {
		t.Errorf("color4 = %d/%d/%d", p.ansi[4].r, p.ansi[4].g, p.ansi[4].b)
	}
}
