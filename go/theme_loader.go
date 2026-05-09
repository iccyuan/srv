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

var hexColorPat = regexp.MustCompile(`(?:#|0x)([0-9a-fA-F]{6})`)

// alacrittyTOMLPalette parses an Alacritty .toml config and returns
// the 16 ANSI colours. Looks for the [colors.normal] and
// [colors.bright] tables; ignores everything else (primary fg/bg,
// cursor, etc.).
//
// We don't pull in a TOML library because Alacritty's colour tables
// are uniform enough to handle with a tiny line scanner. Keys are
// the 8 standard ANSI names; values are quoted strings of the form
// "#hhhhhh" or "0xhhhhhh".
func alacrittyTOMLPalette(data []byte) (*themeColors, error) {
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
		m := hexColorPat.FindStringSubmatch(val)
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

// alacrittyYAMLPalette parses Alacritty's pre-0.13 YAML config.
// Same idea as the TOML cousin: the file always nests
//
//	colors:
//	  normal:
//	    black:   '#...'
//	    ...
//	  bright:
//	    black:   '#...'
//	    ...
//
// We don't bring in a YAML library either -- the structure is so
// uniform that recognising "normal:" / "bright:" headers and the
// 8 colour keys underneath is enough. Any header outside that set
// resets section tracking, so subsequent fg/bg/cursor sub-tables
// don't accidentally map onto colour keys.
func alacrittyYAMLPalette(data []byte) (*themeColors, error) {
	out := &themeColors{}
	section := ""
	found := 0
	for _, line := range strings.Split(string(data), "\n") {
		// Drop comments after a space-prefixed `#`. Quoted strings
		// in our domain don't contain `#` other than for hex, so
		// stripping the trailing comment would risk eating the value;
		// just skip whole-line comments for now.
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		// A YAML header line ends with ':' and has no value.
		if strings.HasSuffix(stripped, ":") {
			name := strings.TrimSpace(strings.TrimSuffix(stripped, ":"))
			switch name {
			case "normal":
				section = "normal"
			case "bright":
				section = "bright"
			default:
				section = ""
			}
			continue
		}
		// `key: value` line.
		eq := strings.Index(stripped, ":")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(stripped[:eq])
		val := stripped[eq+1:]
		m := hexColorPat.FindStringSubmatch(val)
		if m == nil {
			continue
		}
		bright := false
		switch section {
		case "normal":
		case "bright":
			bright = true
		default:
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
		return nil, fmt.Errorf("alacritty yaml: only %d/16 colours found", found)
	}
	return out, nil
}

// kittyPalette parses a Kitty terminal .conf file. Kitty names the
// 16 ANSI colours as `color0` through `color15`, one per line:
//
//	color0  #45475a
//	color1  #f38ba8
//	...
//
// Comments use `#` at line start. Lines that don't match the
// `colorN <hex>` shape are ignored (foreground/background/cursor/...
// are valid Kitty keys, just not what we care about here).
func kittyPalette(data []byte) (*themeColors, error) {
	out := &themeColors{}
	found := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "color") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(fields[0], "color"))
		if err != nil || n < 0 || n > 15 {
			continue
		}
		m := hexColorPat.FindStringSubmatch(fields[1])
		if m == nil {
			continue
		}
		out.ansi[n] = decodeHex(m[1])
		found++
	}
	if found < 8 {
		return nil, fmt.Errorf("kitty conf: only %d/16 colours found", found)
	}
	return out, nil
}

// xresourcesColorPat matches the `<class>.colorN: #hex` (or just
// `colorN: #hex`) pattern used by .Xresources files. The class is
// usually `*`, `URxvt`, or `XTerm`, separated by `.` or `*` from
// the resource name.
var xresourcesColorPat = regexp.MustCompile(`(?i)(?:^|[*.])color(\d+)\s*:\s*(#?[0-9a-fA-F]{6})`)

// xresourcesPalette parses .Xresources / .xresources files used by
// xterm, URxvt, and most classic X terminals:
//
//	*.foreground: #cdd6f4
//	*.color0:     #45475a
//	URxvt.color1: #f38ba8
//	color2:       #a6e3a1
//
// Comments use `!`. We ignore everything that isn't a colorN line.
func xresourcesPalette(data []byte) (*themeColors, error) {
	out := &themeColors{}
	found := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "!") {
			continue
		}
		m := xresourcesColorPat.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 0 || n > 15 {
			continue
		}
		hex := strings.TrimPrefix(m[2], "#")
		if len(hex) != 6 {
			continue
		}
		out.ansi[n] = decodeHex(hex)
		found++
	}
	if found < 8 {
		return nil, fmt.Errorf("xresources: only %d/16 colours found", found)
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
// files share the same basename. Compared lower-case at use time
// so .Xresources / .xresources / .YML / etc all work.
var supportedThemeExts = []string{
	".sh",          // raw shell snippet, highest priority
	".itermcolors", // iTerm2 plist (XML)
	".toml",        // Alacritty TOML (post-0.13)
	".yml",         // Alacritty YAML (legacy, pre-0.13)
	".yaml",        // same; some forks/forks use the longer suffix
	".conf",        // Kitty terminal config
	".xresources",  // xterm / urxvt classic Xresources
}

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
		if t, perr := alacrittyTOMLPalette(data); perr == nil {
			return themeToLSColorsShell(t)
		}
	case ".yml", ".yaml":
		if t, perr := alacrittyYAMLPalette(data); perr == nil {
			return themeToLSColorsShell(t)
		}
	case ".conf":
		if t, perr := kittyPalette(data); perr == nil {
			return themeToLSColorsShell(t)
		}
	case ".xresources":
		if t, perr := xresourcesPalette(data); perr == nil {
			return themeToLSColorsShell(t)
		}
	}
	return ""
}
