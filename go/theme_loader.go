package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// rgbColor is a 24-bit colour split into 0..255 components.
type rgbColor struct{ r, g, b uint8 }

// fg returns the SGR truecolor foreground parameters,
// e.g. "38;2;255;128;0". Caller adds wrapping ESC[ ... m.
func (c rgbColor) fg() string {
	return fmt.Sprintf("38;2;%d;%d;%d", c.r, c.g, c.b)
}

// boldFg prefixes the bold attribute.
func (c rgbColor) boldFg() string { return "01;" + c.fg() }

// themeColors is the 16-entry ANSI palette extracted from a terminal
// emulator's theme file. Index 0..7 = normal black/red/green/yellow/
// blue/magenta/cyan/white, 8..15 = bright variants.
type themeColors struct {
	ansi [16]rgbColor
}

// itermPalette parses an iTerm2 .itermcolors plist (XML) and returns
// the 16 ANSI colours. iTerm encodes each component as <real>
// fractions 0..1; we round to 0..255.
//
// We don't pull in a full plist library -- the format is regular
// enough that walking the string for `<key>Ansi N Color</key>` and
// the three component <real> tags is shorter than wiring up
// encoding/xml with the right struct shape.
func itermPalette(data []byte) (*themeColors, error) {
	src := string(data)
	out := &themeColors{}
	for i := 0; i < 16; i++ {
		marker := fmt.Sprintf(`<key>Ansi %d Color</key>`, i)
		idx := strings.Index(src, marker)
		if idx < 0 {
			return nil, fmt.Errorf("itermcolors: missing Ansi %d", i)
		}
		blockStart := idx + len(marker)
		end := strings.Index(src[blockStart:], "</dict>")
		if end < 0 {
			return nil, fmt.Errorf("itermcolors: malformed Ansi %d block", i)
		}
		block := src[blockStart : blockStart+end]
		out.ansi[i] = rgbColor{
			r: floatToByte(plistFloat(block, "Red Component")),
			g: floatToByte(plistFloat(block, "Green Component")),
			b: floatToByte(plistFloat(block, "Blue Component")),
		}
	}
	return out, nil
}

// plistFloat returns the value of the <real> tag immediately
// following the <key>{name}</key> marker inside `block`, or 0 if
// the key isn't present.
func plistFloat(block, name string) float64 {
	keyMarker := "<key>" + name + "</key>"
	idx := strings.Index(block, keyMarker)
	if idx < 0 {
		return 0
	}
	rest := block[idx+len(keyMarker):]
	open := strings.Index(rest, "<real>")
	if open < 0 {
		return 0
	}
	rest = rest[open+len("<real>"):]
	closeIdx := strings.Index(rest, "</real>")
	if closeIdx < 0 {
		return 0
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(rest[:closeIdx]), 64)
	return f
}

func floatToByte(f float64) uint8 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 255
	}
	return uint8(f*255 + 0.5)
}

var alacrittyHexPat = regexp.MustCompile(`(?:#|0x)([0-9a-fA-F]{6})`)

// alacrittyPalette parses an Alacritty .toml config and returns the
// 16 ANSI colours. Looks for the [colors.normal] and [colors.bright]
// tables; ignores everything else (primary fg/bg, cursor, etc.).
//
// We don't pull in a TOML library because Alacritty's colour tables
// are uniform enough to handle with a tiny line scanner. Keys are
// the 8 standard ANSI names; values are quoted strings of the form
// "#hhhhhh" or "0xhhhhhh".
func alacrittyPalette(data []byte) (*themeColors, error) {
	out := &themeColors{}
	section := ""
	found := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if end := strings.Index(line, "]"); end > 0 {
				section = strings.TrimSpace(line[1:end])
			}
			continue
		}
		bright := false
		switch section {
		case "colors.normal":
		case "colors.bright":
			bright = true
		default:
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := line[eq+1:]
		m := alacrittyHexPat.FindStringSubmatch(val)
		if m == nil {
			continue
		}
		idx := ansiIndex(key, bright)
		if idx < 0 {
			continue
		}
		out.ansi[idx] = decodeHex(m[1])
		found++
	}
	if found < 8 {
		return nil, fmt.Errorf("alacritty toml: only %d/16 colours found", found)
	}
	return out, nil
}

// ansiIndex maps an ANSI colour name (black/red/.../white) to its
// 0..15 index, doubling 8..15 when `bright` is true. Returns -1 for
// unknown names.
func ansiIndex(name string, bright bool) int {
	base := -1
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "black":
		base = 0
	case "red":
		base = 1
	case "green":
		base = 2
	case "yellow":
		base = 3
	case "blue":
		base = 4
	case "magenta":
		base = 5
	case "cyan":
		base = 6
	case "white":
		base = 7
	}
	if base < 0 {
		return -1
	}
	if bright {
		base += 8
	}
	return base
}

func decodeHex(h string) rgbColor {
	n, _ := strconv.ParseUint(h, 16, 32)
	return rgbColor{
		r: uint8(n >> 16),
		g: uint8(n >> 8),
		b: uint8(n),
	}
}

// themeToLSColorsShell turns a parsed 16-colour palette into a shell
// snippet that exports LS_COLORS using truecolor SGR escapes
// (38;2;R;G;B). Standard dircolors category map:
//   - dirs: blue (bold)
//   - links: cyan (bold)
//   - executables: green (bold)
//   - source code: bright-blue (bold)
//   - archives: red
//   - media: magenta
//   - data files: yellow
func themeToLSColorsShell(t *themeColors) string {
	blue := t.ansi[4].boldFg()
	cyan := t.ansi[6].boldFg()
	green := t.ansi[2].boldFg()
	yellow := t.ansi[3].fg()
	red := t.ansi[1].fg()
	magenta := t.ansi[5].fg()
	brightBlue := t.ansi[12].boldFg()

	var sb strings.Builder
	sb.WriteString("export LS_COLORS='no=00:fi=00")
	sb.WriteString(":di=" + blue)
	sb.WriteString(":ln=" + cyan)
	sb.WriteString(":pi=" + yellow)
	sb.WriteString(":so=" + magenta)
	sb.WriteString(":bd=" + yellow + ";01")
	sb.WriteString(":cd=" + yellow + ";01")
	sb.WriteString(":or=01;" + red)
	sb.WriteString(":mi=01;" + red)
	sb.WriteString(":ex=" + green)
	for _, ext := range []string{"tar", "tgz", "gz", "bz2", "xz", "zst", "zip", "7z", "rar", "rpm", "deb", "iso"} {
		sb.WriteString(":*." + ext + "=" + red)
	}
	for _, ext := range []string{"jpg", "jpeg", "png", "gif", "bmp", "svg", "ico", "webp", "mp3", "mp4", "mkv", "avi", "mov", "flac", "wav"} {
		sb.WriteString(":*." + ext + "=" + magenta)
	}
	for _, ext := range []string{"md", "txt", "log", "json", "yaml", "yml", "toml", "conf", "ini", "csv"} {
		sb.WriteString(":*." + ext + "=" + yellow)
	}
	for _, ext := range []string{"go", "py", "js", "ts", "tsx", "jsx", "rs", "c", "h", "cpp", "hpp", "java", "kt", "swift", "rb", "php"} {
		sb.WriteString(":*." + ext + "=" + brightBlue)
	}
	for _, ext := range []string{"sh", "bash", "zsh", "fish"} {
		sb.WriteString(":*." + ext + "=" + green)
	}
	sb.WriteString("'\n")
	return sb.String()
}

// supportedThemeExts is the list of file extensions the user can
// drop into ~/.srv/init/, in order of precedence when multiple
// files share the same basename.
var supportedThemeExts = []string{".sh", ".itermcolors", ".toml"}

// loadThemeFile reads a single theme file from disk and returns the
// shell snippet to inline before remote commands. Empty string when
// the file doesn't exist, can't be parsed, or has an unsupported
// extension -- callers fall back to the next lookup.
func loadThemeFile(p string) string {
	data, err := os.ReadFile(p)
	if err != nil || len(data) == 0 {
		return ""
	}
	switch strings.ToLower(filepath.Ext(p)) {
	case ".sh":
		return string(data)
	case ".itermcolors":
		if t, perr := itermPalette(data); perr == nil {
			return themeToLSColorsShell(t)
		}
	case ".toml":
		if t, perr := alacrittyPalette(data); perr == nil {
			return themeToLSColorsShell(t)
		}
	}
	return ""
}
