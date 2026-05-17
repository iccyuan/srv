package theme

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"srv/internal/picker"
	"srv/internal/session"
	"srv/internal/srvpath"
	"srv/internal/srvtty"
	"strings"
)

// builtinThemes is the bundled directory of colour theme files.
// Each colors/*.sh is a theme; the basename (without .sh) is what
// `srv color use <name>` accepts. Themes shipped here are popular
// dark palettes (dracula, gruvbox, nord, ...) so users can pick
// one that matches their terminal without having to author a
// preset by hand. User files in ~/.srv/init/<name>.sh override a
// built-in of the same name -- copy, tweak, override.
//
//go:embed colors/*.sh
var builtinThemes embed.FS

// BuiltinDefaultTheme names the theme used when no preset is
// selected. The file is special: it tries the user's own dircolors
// db first (~/.dir_colors / ~/.dircolors) before falling back to
// its hardcoded palette, so a hand-tuned remote setup wins over
// our defaults. Other shipped themes activate unconditionally
// because the user explicitly picked them.
const BuiltinDefaultTheme = "dracula"

// BuiltinThemeContent loads colors/<name>.sh from the embedded
// FS. Returns the file body and true on success; "" / false when
// the theme isn't shipped.
func BuiltinThemeContent(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	data, err := builtinThemes.ReadFile("colors/" + name + ".sh")
	if err != nil {
		return "", false
	}
	return string(data), true
}

// BuiltinThemeNames lists the basenames of every shipped theme,
// sorted alphabetically. Used by `srv color list`.
func BuiltinThemeNames() []string {
	entries, err := fs.ReadDir(builtinThemes, "colors")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".sh") {
			out = append(out, strings.TrimSuffix(n, ".sh"))
		}
	}
	sort.Strings(out)
	return out
}

// reservedNames are sentinel values that take the slot of a
// preset name in the session record but mean "use a built-in mode"
// rather than "load a file". Users can't pick them as preset
// filenames -- Cmd's `use` rejects them.
var reservedNames = map[string]bool{
	"on": true, "off": true, "auto": true,
}

// BuiltinPrologue is the prologue used by both `srv color on`
// and the platform-auto path on linux/mac. Strategy:
//
//   - Force colour env (CLICOLOR_FORCE for BSD ls, FORCE_COLOR for
//     Node-flavoured tools, COLORTERM for misc).
//   - LS_COLORS handling:
//   - If the local shell has one set, forward it -- the user's own
//     palette wins.
//   - Otherwise inline the embedded default theme. The theme file
//     itself does `[ -n "$LS_COLORS" ] || export ...`, so a remote
//     that already has a non-empty palette keeps it; only the
//     "exported as empty string" case (some distros' zsh defaults
//     run `eval "$(dircolors -b)"` on a system whose dircolors
//     database is missing, leaving LS_COLORS=""; GNU ls then emits
//     no colour even with --color=always) gets patched.
//   - Use shell functions for the wrapper, not aliases. Functions
//     dispatch at runtime and work in any POSIX-ish shell. Aliases
//     have parse-time semantics that bite us in zsh -c when the
//     definition and the use are in the same parsed unit.
func BuiltinPrologue() string {
	var b strings.Builder
	b.WriteString("export CLICOLOR=1 CLICOLOR_FORCE=1 FORCE_COLOR=1 COLORTERM=truecolor\n")

	// Forward the local shell's LS_COLORS only on linux/mac. Those
	// platforms have a real shell theme convention (vivid, dircolors,
	// distro defaults) and the user's interactive palette is the most
	// natural thing to mirror onto the remote. Windows is excluded:
	// any LS_COLORS env you'd see there typically leaked in via WSL or
	// git-bash and isn't representative of the user's chosen terminal
	// theme, so we use the embedded default instead.
	forwarded := false
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		if v := os.Getenv("LS_COLORS"); v != "" {
			b.WriteString("export LS_COLORS=")
			b.WriteString(srvtty.ShQuote(v))
			b.WriteByte('\n')
			forwarded = true
		}
	}
	if !forwarded {
		if content, ok := BuiltinThemeContent(BuiltinDefaultTheme); ok {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteByte('\n')
			}
		}
	}
	b.WriteString(`ls()    { command ls    --color=always "$@"; }` + "\n")
	b.WriteString(`grep()  { command grep  --color=always "$@"; }` + "\n")
	b.WriteString(`egrep() { command egrep --color=always "$@"; }` + "\n")
	b.WriteString(`fgrep() { command fgrep --color=always "$@"; }` + "\n")
	return b.String()
}

// Prologue resolves the per-session colour selection into the
// shell snippet inlined before a non-TTY CLI command. Resolution:
//
//  1. mode == "off"            -> "" (explicit opt-out)
//  2. mode == "" / "on" / "auto" -> built-in prologue. Default on,
//     any platform.
//  3. mode == <preset name>    -> first try ~/.srv/init/<name>.sh
//     (user-authored), then a shipped built-in theme of that name.
//     If neither exists, fall back to the default prologue.
//
// MCP never goes through this path; it stays plain text.
func Prologue() string {
	mode := session.GetColorPreset()
	switch mode {
	case "off":
		return ""
	case "", "on", "auto":
		return BuiltinPrologue()
	}

	body := LoadPresetBody(mode)
	if body == "" {
		// Stale pin -- the file or built-in vanished. Don't lose
		// colour silently; reuse the default.
		return BuiltinPrologue()
	}

	var b strings.Builder
	b.WriteString("export CLICOLOR=1 CLICOLOR_FORCE=1 FORCE_COLOR=1 COLORTERM=truecolor\n")
	b.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(`ls()    { command ls    --color=always "$@"; }` + "\n")
	b.WriteString(`grep()  { command grep  --color=always "$@"; }` + "\n")
	b.WriteString(`egrep() { command egrep --color=always "$@"; }` + "\n")
	b.WriteString(`fgrep() { command fgrep --color=always "$@"; }` + "\n")
	return b.String()
}

// UserPresetExt returns the first matching extension for a user-
// supplied theme file at ~/.srv/init/<name><ext>, in precedence order.
// "" when no file of that name exists.
func UserPresetExt(name string) string {
	dir := srvpath.ColorPresetsDir()
	for _, ext := range SupportedExts {
		if _, err := os.Stat(filepath.Join(dir, name+ext)); err == nil {
			return ext
		}
	}
	return ""
}

// LoadPresetBody returns the shell snippet for a named preset.
// Lookup order:
//
//  1. ~/.srv/init/<name>.sh         -- raw shell snippet (highest)
//  2. ~/.srv/init/<name>.itermcolors -- iTerm2 plist, parsed and
//     converted to LS_COLORS truecolor on the fly
//  3. ~/.srv/init/<name>.toml        -- Alacritty TOML, same idea
//  4. embedded colors/<name>.sh     -- shipped built-in
//
// Empty string if nothing matches.
func LoadPresetBody(name string) string {
	dir := srvpath.ColorPresetsDir()
	for _, ext := range SupportedExts {
		if body := LoadFile(filepath.Join(dir, name+ext)); body != "" {
			return body
		}
	}
	if content, ok := BuiltinThemeContent(name); ok {
		return content
	}
	return ""
}

// ApplyPreset validates `name` against user presets + built-ins,
// persists it as the active colour preset for this shell, and prints
// the same one-line confirmation `srv color use <name>` always has.
// Shared between the explicit `srv color use <name>` form and the TTY
// picker fallback.
func ApplyPreset(name string) error {
	if reservedNames[name] {
		return fmt.Errorf("color use: %q is a reserved mode name; use `srv color %s` directly.", name, name)
	}
	userExt := UserPresetExt(name)
	_, builtin := BuiltinThemeContent(name)
	if userExt == "" && !builtin {
		return fmt.Errorf(
			"color use: %q not found in %s (looked for *.sh / *.itermcolors / *.toml) and no built-in theme matches.\nlist available with `srv color list`.",
			name, srvpath.ColorPresetsDir())
	}
	sid, err := session.SetColorPreset(name)
	if err != nil {
		return fmt.Errorf("color use: %v", err)
	}
	origin := "built-in"
	if userExt != "" {
		origin = "user " + userExt
	}
	fmt.Printf("color: using %s preset %q (session=%s)\n", origin, name, sid)
	return nil
}

// BuildPickerItems lists every selectable colour preset for the TTY
// picker: user files in ~/.srv/init/ first (extension shown in the meta
// column), then built-ins (skipping any whose name is shadowed by a user
// file -- the user version wins, just like LoadPresetBody). isPinned
// marks the currently active preset (if any). isDefault marks the
// shipped default theme (dracula).
func BuildPickerItems() []*picker.Item {
	active := session.GetColorPreset()
	userPresets, _ := ListPresets()
	out := make([]*picker.Item, 0, len(userPresets)+8)
	overridden := map[string]bool{}
	for _, p := range userPresets {
		overridden[p] = true
		ext := UserPresetExt(p)
		out = append(out, &picker.Item{
			Name:      p,
			Meta:      "user " + ext,
			IsPinned:  p == active,
			IsDefault: false,
		})
	}
	for _, p := range BuiltinThemeNames() {
		if overridden[p] {
			continue
		}
		out = append(out, &picker.Item{
			Name:      p,
			Meta:      "built-in",
			IsPinned:  p == active,
			IsDefault: p == BuiltinDefaultTheme,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Cmd implements `srv color [on|off|auto|use <name>|list|status]`.
//
//   - on / off / auto: simple toggles -- no preset file needed.
//     "on" forces colour on regardless of local OS, "off" forces off,
//     "auto" (the default) is on for linux/mac local and off for
//     windows local.
//   - use <name>: load ~/.srv/init/<name>.sh as the prologue; lets a
//     power user fully customise. Reserved names (on/off/auto) are
//     rejected.
//   - list: enumerate ~/.srv/init/*.sh by basename, marking the
//     active preset.
//   - status (default): print which mode + prologue source is live.
func Cmd(args []string) error {
	action := "status"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}
	switch action {
	case "on":
		sid, err := session.SetColorPreset("on")
		if err != nil {
			fmt.Fprintln(os.Stderr, "color on:", err)
			return fmt.Errorf("")
		}
		fmt.Printf("color: on (session=%s)\n", sid)
		return nil
	case "off", "disable":
		sid, err := session.SetColorPreset("off")
		if err != nil {
			fmt.Fprintln(os.Stderr, "color off:", err)
			return fmt.Errorf("")
		}
		fmt.Printf("color: off (session=%s)\n", sid)
		return nil
	case "auto", "clear", "default":
		sid, err := session.SetColorPreset("")
		if err != nil {
			fmt.Fprintln(os.Stderr, "color auto:", err)
			return fmt.Errorf("")
		}
		fmt.Printf("color: auto (session=%s)\n", sid)
		return nil
	case "list":
		userPresets, err := ListPresets()
		if err != nil {
			fmt.Fprintln(os.Stderr, "color list:", err)
			return fmt.Errorf("")
		}
		userSet := map[string]bool{}
		for _, p := range userPresets {
			userSet[p] = true
		}
		active := session.GetColorPreset()
		mark := func(name string) string {
			if name == active {
				return "* "
			}
			return "  "
		}
		if len(userPresets) > 0 {
			fmt.Printf("user (%s):\n", srvpath.ColorPresetsDir())
			for _, p := range userPresets {
				ext := UserPresetExt(p)
				fmt.Printf("  %s%-24s %s\n", mark(p), p, ext)
			}
		}
		fmt.Println("built-in:")
		for _, p := range BuiltinThemeNames() {
			suffix := ""
			if userSet[p] {
				suffix = "(overridden by user file)"
			} else if p == BuiltinDefaultTheme {
				suffix = "(default)"
			}
			fmt.Printf("  %s%-24s %s\n", mark(p), p, suffix)
		}
		fmt.Println()
		fmt.Println("supported user formats: .sh / .itermcolors / .toml / .yml / .conf (Kitty) / .Xresources")
		fmt.Println("apply with: srv color use <name>")
		return nil
	case "use":
		if len(args) < 2 || args[1] == "" {
			// TTY: open the same picker `srv use` uses, populated with
			// every named preset (user files first, then built-ins,
			// user wins on name collision). Off-TTY keeps the old usage
			// error so scripts still get a clean signal.
			if srvtty.IsStdinTTY() {
				items := BuildPickerItems()
				if len(items) == 0 {
					fmt.Fprintln(os.Stderr, "(no colour presets available)")
					return fmt.Errorf("")
				}
				sel, ok := picker.Run(items, "Select a colour preset for this shell:", picker.Labels{Pin: "active", Def: "default"})
				if !ok {
					return nil
				}
				return ApplyPreset(sel)
			}
			fmt.Fprintln(os.Stderr, "usage: srv color use <name>")
			fmt.Fprintln(os.Stderr, "list available with `srv color list`.")
			return fmt.Errorf("")
		}
		return ApplyPreset(args[1])
	case "status", "":
		sid := session.ID()
		mode := session.GetColorPreset()
		switch mode {
		case "on":
			fmt.Printf("color: on (session=%s)\n", sid)
		case "off":
			fmt.Printf("color: off (session=%s)\n", sid)
		case "", "auto":
			fmt.Printf("color: on by default (session=%s; `srv color off` to disable)\n", sid)
		default:
			origin := "missing"
			location := ""
			if ext := UserPresetExt(mode); ext != "" {
				origin = "user " + ext
				location = " at " + filepath.Join(srvpath.ColorPresetsDir(), mode+ext)
			} else if _, ok := BuiltinThemeContent(mode); ok {
				origin = "built-in"
			}
			fmt.Printf("color: %s preset %q%s (session=%s)\n", origin, mode, location, sid)
		}
		return nil
	}
	fmt.Fprintln(os.Stderr, "usage: srv color [on|off|auto|use <name>|list|status]")
	return fmt.Errorf("")
}
