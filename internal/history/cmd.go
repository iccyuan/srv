package history

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"srv/internal/srvutil"
	"strconv"
	"strings"
)

const usage = `usage: srv history [list] [-n N] [--profile P] [--grep RE] [--json] [--all]
  srv history                  show recent commands across this shell's session
  srv history --all            include every session's commands
  srv history -n 100           limit to last N entries (default 50)
  srv history --profile prod   filter by profile
  srv history --grep "kill"    filter by command regex
  srv history --json           emit raw JSONL
  srv history path             print the on-disk path
  srv history clear            truncate the history file`

// Cmd implements `srv history ...`. Always read-only outside of `clear`.
func Cmd(args []string, currentSession string) error {
	// Subcommand shortcuts.
	if len(args) > 0 {
		switch args[0] {
		case "path":
			fmt.Println(Path())
			return nil
		case "clear":
			if err := Clear(); err != nil {
				return srvutil.Errf(1, "error: %v", err)
			}
			fmt.Println("cleared")
			return nil
		case "help", "-h", "--help":
			fmt.Println(usage)
			return nil
		}
	}

	opts := parseFlags(args)
	entries, err := ReadAll()
	if err != nil {
		return srvutil.Errf(1, "error: %v", err)
	}

	// Filtering.
	filtered := entries[:0]
	for _, e := range entries {
		if opts.profile != "" && e.Profile != opts.profile {
			continue
		}
		if !opts.all && currentSession != "" && e.Session != "" && e.Session != currentSession {
			continue
		}
		if opts.re != nil && !opts.re.MatchString(e.Cmd) {
			continue
		}
		filtered = append(filtered, e)
	}

	// Tail to limit.
	if opts.limit > 0 && len(filtered) > opts.limit {
		filtered = filtered[len(filtered)-opts.limit:]
	}

	if opts.jsonOut {
		for _, e := range filtered {
			b, _ := json.Marshal(e)
			fmt.Println(string(b))
		}
		return nil
	}

	if len(filtered) == 0 {
		if opts.all {
			fmt.Println("(no history)")
		} else {
			fmt.Println("(no history for this session -- try `srv history --all`)")
		}
		return nil
	}
	for _, e := range filtered {
		mark := " "
		if e.Exit != 0 {
			mark = "!"
		}
		when := e.Time
		if len(when) > 19 {
			when = when[:19]
		}
		prof := e.Profile
		if prof == "" {
			prof = "-"
		}
		fmt.Fprintf(os.Stdout, "%s %s [%s] %s\n", mark, when, prof, e.Cmd)
	}
	return nil
}

type histOpts struct {
	limit   int
	profile string
	re      *regexp.Regexp
	jsonOut bool
	all     bool
}

func parseFlags(args []string) histOpts {
	o := histOpts{limit: 50}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "list":
			// no-op, default action
		case a == "-n" || a == "--limit":
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					o.limit = n
				}
				i++
			}
		case strings.HasPrefix(a, "-n="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "-n=")); err == nil && n > 0 {
				o.limit = n
			}
		case strings.HasPrefix(a, "--limit="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err == nil && n > 0 {
				o.limit = n
			}
		case a == "--profile":
			if i+1 < len(args) {
				o.profile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--profile="):
			o.profile = strings.TrimPrefix(a, "--profile=")
		case a == "--grep":
			if i+1 < len(args) {
				if re, err := regexp.Compile(args[i+1]); err == nil {
					o.re = re
				}
				i++
			}
		case strings.HasPrefix(a, "--grep="):
			if re, err := regexp.Compile(strings.TrimPrefix(a, "--grep=")); err == nil {
				o.re = re
			}
		case a == "--json":
			o.jsonOut = true
		case a == "--all":
			o.all = true
		}
	}
	return o
}
