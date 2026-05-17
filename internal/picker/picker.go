package picker

import (
	"srv/internal/config"
	"srv/internal/srvtty"

	"fmt"
	"os"
	"sort"
	"srv/internal/session"
	"srv/internal/srvutil"
	"strings"
	"time"

	"golang.org/x/term"
)

// Interactive picker. Triggered by `srv use` (no arg, TTY),
// `srv config default` (no arg, TTY), and `srv color use` (no arg, TTY).
// Pure stdlib + golang.org/x/term -- no TUI library, no extra deps.
// Output goes to stderr so the picker works even when stdout is
// redirected.
//
// Visual contract:
//   * isPinned -- yellow marker, label supplied by caller (e.g.
//     "[this shell]" for profiles, "[active]" for colour presets)
//   * isDefault -- cyan marker, label supplied by caller (e.g.
//     "[default]" for both)
//   * highlighted row uses reverse video plus a `>` cursor
//   * footer hint line dims out the keybindings
//
// Both markers can apply to the same row.

type Item struct {
	Name      string
	Meta      string // free-form right-column descriptor (host:port, "built-in", etc.)
	IsPinned  bool   // current selection (e.g. session pin, active colour preset)
	IsDefault bool   // baseline default (global default profile, default theme)
}

// Labels lets the caller customise the marker text rendered for
// isPinned (yellow) and isDefault (cyan) rows. Both fields are the inner
// text without the surrounding [] brackets.
type Labels struct {
	Pin string
	Def string
}

// ProfileLabels matches the original profile-picker visual.
var ProfileLabels = Labels{Pin: "this shell", Def: "default"}

// BuildItems translates the config + current session into picker rows.
func BuildItems(cfg *config.Config) []*Item {
	_, rec := session.Touch()
	pinned := ""
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	out := make([]*Item, 0, len(cfg.Profiles))
	for name, p := range cfg.Profiles {
		conn := p.Host
		if p.User != "" {
			conn = p.User + "@" + p.Host
		}
		if p.GetPort() != 22 {
			conn = fmt.Sprintf("%s:%d", conn, p.GetPort())
		}
		out = append(out, &Item{
			Name:      name,
			Meta:      conn,
			IsPinned:  name == pinned,
			IsDefault: name == cfg.DefaultProfile,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RunProfile is a thin wrapper preserving the original API for the
// profile-selection use sites.
func RunProfile(items []*Item, prompt string) (string, bool) {
	return Run(items, prompt, ProfileLabels)
}

// Run runs the interactive picker with caller-supplied marker
// labels. Returns ("", false) on cancel, (name, true) on selection.
// Caller must ensure stdin is a TTY.
func Run(items []*Item, prompt string, labels Labels) (string, bool) {
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "(no profiles configured -- run `srv init`)")
		return "", false
	}

	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		// No raw mode -> can't drive the picker. Print the list and bail.
		fmt.Fprintln(os.Stderr, "interactive picker unavailable:", err)
		return "", false
	}
	defer term.Restore(fd, state)
	fmt.Fprint(os.Stderr, srvutil.Hide)
	defer fmt.Fprint(os.Stderr, srvutil.Show)

	kr := srvtty.NewKeyReader()

	// Start cursor on a meaningful row: prefer pinned, then default, else 0.
	cursor := 0
	for i, it := range items {
		if it.IsPinned {
			cursor = i
			break
		}
	}
	if cursor == 0 && !items[0].IsPinned {
		for i, it := range items {
			if it.IsDefault {
				cursor = i
				break
			}
		}
	}

	filter := ""
	filterMode := false
	prevLines := 0

	for {
		view := filterItems(items, filter)
		if cursor >= len(view) {
			cursor = len(view) - 1
		}
		if cursor < 0 {
			cursor = 0
		}

		if prevLines > 0 {
			fmt.Fprintf(os.Stderr, "\x1b[%dA\x1b[J", prevLines)
		}
		prevLines = drawPicker(prompt, view, cursor, filter, filterMode, labels)

		b, ok := kr.Read()
		if !ok {
			return cancelPicker(prevLines)
		}

		if filterMode {
			switch b {
			case 0x1b: // ESC -- exit filter mode (don't try to peek; in
				// filter mode we never produce arrow keys).
				filterMode = false
			case '\r', '\n':
				if len(view) == 0 {
					continue
				}
				clearPicker(prevLines)
				return view[cursor].Name, true
			case 0x7f, 0x08: // backspace / DEL
				if filter != "" {
					filter = filter[:len(filter)-1]
				}
				cursor = 0
			case 0x03: // Ctrl-C
				return cancelPicker(prevLines)
			default:
				if b >= 0x20 && b < 0x7f {
					filter += string(b)
					cursor = 0
				}
			}
			continue
		}

		switch b {
		case 'q', 0x03: // q / Ctrl-C
			return cancelPicker(prevLines)
		case 'k':
			if cursor > 0 {
				cursor--
			}
		case 'j':
			if cursor < len(view)-1 {
				cursor++
			}
		case '\r', '\n':
			if len(view) == 0 {
				continue
			}
			clearPicker(prevLines)
			return view[cursor].Name, true
		case '/':
			filterMode = true
			filter = ""
		case 0x1b:
			// Possibly an arrow-key escape sequence: ESC [ A/B/C/D.
			// Brief peek with timeout so a standalone ESC still cancels.
			b2, more := kr.ReadWithTimeout(80 * time.Millisecond)
			if !more {
				return cancelPicker(prevLines)
			}
			if b2 != '[' {
				continue
			}
			b3, more := kr.ReadWithTimeout(20 * time.Millisecond)
			if !more {
				continue
			}
			switch b3 {
			case 'A':
				if cursor > 0 {
					cursor--
				}
			case 'B':
				if cursor < len(view)-1 {
					cursor++
				}
			}
		}
	}
}

func cancelPicker(lines int) (string, bool) {
	clearPicker(lines)
	fmt.Fprintln(os.Stderr, "(cancelled)")
	return "", false
}

func clearPicker(lines int) {
	if lines > 0 {
		fmt.Fprintf(os.Stderr, "\x1b[%dA\x1b[J", lines)
	}
}

func filterItems(items []*Item, filter string) []*Item {
	if filter == "" {
		return items
	}
	q := strings.ToLower(filter)
	out := make([]*Item, 0, len(items))
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.Name), q) ||
			strings.Contains(strings.ToLower(it.Meta), q) {
			out = append(out, it)
		}
	}
	return out
}

// drawPicker emits the full picker UI to stderr and returns the line
// count it produced (so the next pass knows how far to scroll up).
func drawPicker(prompt string, items []*Item, cursor int, filter string, filterMode bool, labels Labels) int {
	w := os.Stderr
	lines := 0

	fmt.Fprintf(w, "%s%s%s\r\n", srvutil.Bold+srvutil.Cyan, prompt, srvutil.Reset)
	lines++

	if filterMode {
		fmt.Fprintf(w, "%sFilter:%s %s_\r\n", srvutil.Bold, srvutil.Reset, filter)
		lines++
	}

	if len(items) == 0 {
		fmt.Fprint(w, "  (no matches)\r\n")
		lines++
	}

	maxName := 0
	for _, it := range items {
		if n := len(it.Name); n > maxName {
			maxName = n
		}
	}
	if maxName < 8 {
		maxName = 8
	}

	for i, it := range items {
		marker := "  "
		if i == cursor {
			marker = srvutil.Cyan + srvutil.Bold + "> " + srvutil.Reset
		}
		row := fmt.Sprintf("%-*s  %s", maxName, it.Name, it.Meta)
		if it.IsPinned {
			row += " " + srvutil.Yellow + srvutil.Bold + "[" + labels.Pin + "]" + srvutil.Reset
		}
		if it.IsDefault {
			row += " " + srvutil.Cyan + srvutil.Bold + "[" + labels.Def + "]" + srvutil.Reset
		}
		if i == cursor {
			fmt.Fprintf(w, "%s%s%s%s\r\n", marker, srvutil.Reverse+srvutil.Bold, row, srvutil.Reset)
		} else {
			fmt.Fprintf(w, "%s%s\r\n", marker, row)
		}
		lines++
	}

	hint := "up/down or j/k  ENTER select  / filter  q cancel"
	if filterMode {
		hint = "type to filter  ENTER select  ESC exit filter  Ctrl-C cancel"
	}
	fmt.Fprintf(w, "%s%s%s\r\n", srvutil.Dim, hint, srvutil.Reset)
	lines++

	return lines
}
