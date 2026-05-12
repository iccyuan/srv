package main

import (
	"bufio"
	"fmt"
	"os"
	"sort"
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

const (
	ansiReset   = "\x1b[0m"
	ansiBold    = "\x1b[1m"
	ansiDim     = "\x1b[2m"
	ansiReverse = "\x1b[7m"
	ansiRed     = "\x1b[31m"
	ansiGreen   = "\x1b[32m"
	ansiYellow  = "\x1b[33m"
	ansiBlue    = "\x1b[34m"
	ansiMagenta = "\x1b[35m"
	ansiCyan    = "\x1b[36m"
	ansiHide    = "\x1b[?25l"
	ansiShow    = "\x1b[?25h"
)

type pickerItem struct {
	name      string
	meta      string // free-form right-column descriptor (host:port, "built-in", etc.)
	isPinned  bool   // current selection (e.g. session pin, active colour preset)
	isDefault bool   // baseline default (global default profile, default theme)
}

// pickerLabels lets the caller customise the marker text rendered for
// isPinned (yellow) and isDefault (cyan) rows. Both fields are the inner
// text without the surrounding [] brackets.
type pickerLabels struct {
	pin string
	def string
}

// profilePickerLabels matches the original profile-picker visual.
var profilePickerLabels = pickerLabels{pin: "this shell", def: "default"}

// buildPickerItems translates the config + current session into picker rows.
func buildPickerItems(cfg *Config) []*pickerItem {
	_, rec := TouchSession()
	pinned := ""
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	out := make([]*pickerItem, 0, len(cfg.Profiles))
	for name, p := range cfg.Profiles {
		conn := p.Host
		if p.User != "" {
			conn = p.User + "@" + p.Host
		}
		if p.GetPort() != 22 {
			conn = fmt.Sprintf("%s:%d", conn, p.GetPort())
		}
		out = append(out, &pickerItem{
			name:      name,
			meta:      conn,
			isPinned:  name == pinned,
			isDefault: name == cfg.DefaultProfile,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// runProfilePicker is a thin wrapper preserving the original API for the
// profile-selection use sites.
func runProfilePicker(items []*pickerItem, prompt string) (string, bool) {
	return runItemPicker(items, prompt, profilePickerLabels)
}

// runItemPicker runs the interactive picker with caller-supplied marker
// labels. Returns ("", false) on cancel, (name, true) on selection.
// Caller must ensure stdin is a TTY.
func runItemPicker(items []*pickerItem, prompt string, labels pickerLabels) (string, bool) {
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
	fmt.Fprint(os.Stderr, ansiHide)
	defer fmt.Fprint(os.Stderr, ansiShow)

	kr := newKeyReader()

	// Start cursor on a meaningful row: prefer pinned, then default, else 0.
	cursor := 0
	for i, it := range items {
		if it.isPinned {
			cursor = i
			break
		}
	}
	if cursor == 0 && !items[0].isPinned {
		for i, it := range items {
			if it.isDefault {
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

		b, ok := kr.read()
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
				return view[cursor].name, true
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
			return view[cursor].name, true
		case '/':
			filterMode = true
			filter = ""
		case 0x1b:
			// Possibly an arrow-key escape sequence: ESC [ A/B/C/D.
			// Brief peek with timeout so a standalone ESC still cancels.
			b2, more := kr.readWithTimeout(80 * time.Millisecond)
			if !more {
				return cancelPicker(prevLines)
			}
			if b2 != '[' {
				continue
			}
			b3, more := kr.readWithTimeout(20 * time.Millisecond)
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

func filterItems(items []*pickerItem, filter string) []*pickerItem {
	if filter == "" {
		return items
	}
	q := strings.ToLower(filter)
	out := make([]*pickerItem, 0, len(items))
	for _, it := range items {
		if strings.Contains(strings.ToLower(it.name), q) ||
			strings.Contains(strings.ToLower(it.meta), q) {
			out = append(out, it)
		}
	}
	return out
}

// drawPicker emits the full picker UI to stderr and returns the line
// count it produced (so the next pass knows how far to scroll up).
func drawPicker(prompt string, items []*pickerItem, cursor int, filter string, filterMode bool, labels pickerLabels) int {
	w := os.Stderr
	lines := 0

	fmt.Fprintf(w, "%s%s%s\r\n", ansiBold+ansiCyan, prompt, ansiReset)
	lines++

	if filterMode {
		fmt.Fprintf(w, "%sFilter:%s %s_\r\n", ansiBold, ansiReset, filter)
		lines++
	}

	if len(items) == 0 {
		fmt.Fprint(w, "  (no matches)\r\n")
		lines++
	}

	maxName := 0
	for _, it := range items {
		if n := len(it.name); n > maxName {
			maxName = n
		}
	}
	if maxName < 8 {
		maxName = 8
	}

	for i, it := range items {
		marker := "  "
		if i == cursor {
			marker = ansiCyan + ansiBold + "> " + ansiReset
		}
		row := fmt.Sprintf("%-*s  %s", maxName, it.name, it.meta)
		if it.isPinned {
			row += " " + ansiYellow + ansiBold + "[" + labels.pin + "]" + ansiReset
		}
		if it.isDefault {
			row += " " + ansiCyan + ansiBold + "[" + labels.def + "]" + ansiReset
		}
		if i == cursor {
			fmt.Fprintf(w, "%s%s%s%s\r\n", marker, ansiReverse+ansiBold, row, ansiReset)
		} else {
			fmt.Fprintf(w, "%s%s\r\n", marker, row)
		}
		lines++
	}

	hint := "up/down or j/k  ENTER select  / filter  q cancel"
	if filterMode {
		hint = "type to filter  ENTER select  ESC exit filter  Ctrl-C cancel"
	}
	fmt.Fprintf(w, "%s%s%s\r\n", ansiDim, hint, ansiReset)
	lines++

	return lines
}

// keyReader pumps stdin bytes into a buffered channel so the main loop
// can read with a timeout (needed to disambiguate standalone ESC from an
// arrow-key escape sequence). The pumping goroutine outlives the picker
// for the rest of the process; that's fine for a one-shot CLI tool.
type keyReader struct {
	ch chan byte
}

func newKeyReader() *keyReader {
	kr := &keyReader{ch: make(chan byte, 16)}
	go func() {
		rd := bufio.NewReader(os.Stdin)
		for {
			b, err := rd.ReadByte()
			if err != nil {
				close(kr.ch)
				return
			}
			kr.ch <- b
		}
	}()
	return kr
}

func (kr *keyReader) read() (byte, bool) {
	b, ok := <-kr.ch
	return b, ok
}

func (kr *keyReader) readWithTimeout(d time.Duration) (byte, bool) {
	select {
	case b, ok := <-kr.ch:
		if !ok {
			return 0, false
		}
		return b, true
	case <-time.After(d):
		return 0, false
	}
}
