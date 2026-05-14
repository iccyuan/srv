// Package recipe implements `srv recipe`: named multi-step remote
// playbooks with positional ($1..$N) and named (${KEY}) parameter
// substitution. Storage lives in Config.Recipes; this package wraps
// CRUD + the run-time executor.
//
// Each recipe is one ordered list of shell commands. They run against
// the profile resolved at execution time (or the recipe's pinned
// profile, when set). The executor goes through the normal
// remote.RunStream path so colour / env / cwd / hooks behave exactly
// like a direct `srv <cmd>` would.
package recipe

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/remote"
	"strings"
)

// Cmd dispatches `srv recipe ...`.
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return list(cfg)
	}
	switch args[0] {
	case "list":
		return list(cfg)
	case "show":
		if len(args) < 2 {
			return clierr.Errf(2, "usage: srv recipe show <name>")
		}
		return show(cfg, args[1])
	case "save", "add":
		return save(args[1:], cfg, profileOverride)
	case "rm", "remove":
		if len(args) < 2 {
			return clierr.Errf(2, "usage: srv recipe rm <name>")
		}
		return remove(cfg, args[1])
	case "run":
		if len(args) < 2 {
			return clierr.Errf(2, "usage: srv recipe run <name> [args...]")
		}
		return run(args[1:], cfg, profileOverride)
	}
	return clierr.Errf(2, `usage: srv recipe [list|show|save|rm|run] [args]
  srv recipe save <name> [--profile P] [--ignore-errors] [--desc "..."] -- cmd1 ;; cmd2 ;; ...
  srv recipe run <name> [pos1 pos2 ...] [KEY=value ...]
  srv recipe show <name>
  srv recipe list / rm <name>`)
}

func list(cfg *config.Config) error {
	if len(cfg.Recipes) == 0 {
		fmt.Println("(no recipes saved)")
		return nil
	}
	names := make([]string, 0, len(cfg.Recipes))
	for n := range cfg.Recipes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := cfg.Recipes[n]
		desc := r.Description
		if desc == "" {
			desc = fmt.Sprintf("%d step(s)", len(r.Steps))
		}
		flags := ""
		if r.Profile != "" {
			flags += " profile=" + r.Profile
		}
		if r.IgnoreErrors {
			flags += " ignore-errors"
		}
		fmt.Printf("%-20s %s%s\n", n, desc, flags)
	}
	return nil
}

func show(cfg *config.Config, name string) error {
	r, ok := cfg.Recipes[name]
	if !ok {
		return clierr.Errf(1, "no recipe %q", name)
	}
	fmt.Printf("name:        %s\n", name)
	if r.Description != "" {
		fmt.Printf("description: %s\n", r.Description)
	}
	if r.Profile != "" {
		fmt.Printf("profile:     %s (pinned)\n", r.Profile)
	}
	fmt.Printf("ignore-errors: %v\n", r.IgnoreErrors)
	fmt.Println("steps:")
	for i, s := range r.Steps {
		fmt.Printf("  [%d] %s\n", i, s)
	}
	vars := collectVars(r.Steps)
	if len(vars) > 0 {
		fmt.Printf("variables:   %s\n", strings.Join(vars, ", "))
	}
	return nil
}

// save parses `srv recipe save <name> [--profile P] [--ignore-errors]
// [--desc "..."] -- cmd1 ;; cmd2 ;; ...`. Steps are separated by `;;`
// (rather than `;`) so the user can still write shell-`;` inside one
// step. Without explicit step markers, the whole tail string is one
// step.
func save(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return clierr.Errf(2, "usage: srv recipe save <name> [...] -- <step...>")
	}
	name := args[0]
	rest := args[1:]
	rec := &config.Recipe{Profile: profileOverride}
	dashSeen := false
	tail := []string{}
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if dashSeen {
			tail = append(tail, a)
			continue
		}
		switch {
		case a == "--":
			dashSeen = true
		case a == "--profile":
			if i+1 >= len(rest) {
				return clierr.Errf(2, "--profile needs a value")
			}
			rec.Profile = rest[i+1]
			i++
		case strings.HasPrefix(a, "--profile="):
			rec.Profile = strings.TrimPrefix(a, "--profile=")
		case a == "--desc" || a == "--description":
			if i+1 >= len(rest) {
				return clierr.Errf(2, "--desc needs a value")
			}
			rec.Description = rest[i+1]
			i++
		case strings.HasPrefix(a, "--desc="):
			rec.Description = strings.TrimPrefix(a, "--desc=")
		case strings.HasPrefix(a, "--description="):
			rec.Description = strings.TrimPrefix(a, "--description=")
		case a == "--ignore-errors":
			rec.IgnoreErrors = true
		default:
			return clierr.Errf(2, "unexpected arg %q before --", a)
		}
	}
	if len(tail) == 0 {
		return clierr.Errf(2, "no steps provided (put step text after `--`, separate multiple with `;;`)")
	}
	rec.Steps = parseSteps(tail)
	if rec.Profile != "" {
		if _, ok := cfg.Profiles[rec.Profile]; !ok {
			return clierr.Errf(1, "pinned profile %q not found", rec.Profile)
		}
	}
	if cfg.Recipes == nil {
		cfg.Recipes = map[string]*config.Recipe{}
	}
	cfg.Recipes[name] = rec
	if err := config.Save(cfg); err != nil {
		return clierr.Errf(1, "save: %v", err)
	}
	fmt.Printf("saved recipe %q (%d step(s))\n", name, len(rec.Steps))
	return nil
}

// parseSteps splits the post-`--` tail into individual step strings.
// Steps are separated by `;;`; whitespace-only steps are dropped.
func parseSteps(tail []string) []string {
	joined := strings.Join(tail, " ")
	parts := strings.Split(joined, ";;")
	out := []string{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func remove(cfg *config.Config, name string) error {
	if _, ok := cfg.Recipes[name]; !ok {
		return clierr.Errf(1, "no recipe %q", name)
	}
	delete(cfg.Recipes, name)
	if err := config.Save(cfg); err != nil {
		return clierr.Errf(1, "save: %v", err)
	}
	fmt.Printf("removed recipe %q\n", name)
	return nil
}

// run executes the recipe steps. positional args fill $1..$N;
// `KEY=value` args feed ${KEY} substitution. Empty arguments are
// permitted -- referencing an unset ${KEY} just expands to "".
func run(args []string, cfg *config.Config, profileOverride string) error {
	name := args[0]
	rec, ok := cfg.Recipes[name]
	if !ok {
		return clierr.Errf(1, "no recipe %q", name)
	}
	posArgs := []string{}
	kwargs := map[string]string{}
	for _, a := range args[1:] {
		if i := strings.Index(a, "="); i > 0 && validVarName(a[:i]) {
			kwargs[a[:i]] = a[i+1:]
			continue
		}
		posArgs = append(posArgs, a)
	}

	// Profile resolution: explicit -P override > recipe's pinned
	// profile > whatever Resolve picks (session pin / default).
	prof := profileOverride
	if prof == "" {
		prof = rec.Profile
	}
	resolvedName, profile, err := config.Resolve(cfg, prof)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}
	cwd := config.GetCwd(resolvedName, profile)

	fmt.Fprintf(os.Stderr, "recipe %q -> %s (%d step(s))\n\n", name, resolvedName, len(rec.Steps))
	for i, raw := range rec.Steps {
		step := substitute(raw, posArgs, kwargs)
		fmt.Fprintf(os.Stderr, "[step %d/%d] %s\n", i+1, len(rec.Steps), step)
		rc := remote.RunStream(profile, cwd, remote.ApplyEnv(profile, step), false)
		if rc != 0 {
			fmt.Fprintf(os.Stderr, "  -> exit %d\n\n", rc)
			if !rec.IgnoreErrors {
				return clierr.Code(rc)
			}
		} else {
			fmt.Fprintln(os.Stderr)
		}
	}
	return nil
}

// substitute replaces $1..$9 / ${KEY} / $NAME references in the raw
// command. Unset references collapse to "". This is intentionally
// simpler than full shell parameter expansion (no defaults, no
// indirection); recipes that need that should hand-roll a wrapper.
func substitute(raw string, pos []string, kw map[string]string) string {
	out := raw
	// Positional $1..$9.
	for i := 1; i <= 9; i++ {
		val := ""
		if i-1 < len(pos) {
			val = pos[i-1]
		}
		out = strings.ReplaceAll(out, fmt.Sprintf("$%d", i), val)
	}
	// Named ${KEY} and $KEY (alphanumeric + underscore).
	out = bracedVarRE.ReplaceAllStringFunc(out, func(m string) string {
		key := m[2 : len(m)-1]
		return kw[key]
	})
	out = bareVarRE.ReplaceAllStringFunc(out, func(m string) string {
		key := m[1:]
		return kw[key]
	})
	return out
}

// bracedVarRE matches `${KEY}`; bareVarRE matches `$KEY` (must be
// followed by non-word boundary). Positional `$1..$9` are handled
// separately so they don't fall through into bareVarRE.
var (
	bracedVarRE = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)
	bareVarRE   = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*`)
)

// collectVars enumerates every $1..$9 / ${KEY} / $KEY reference in
// steps so `srv recipe show` can tell the user what params they need.
func collectVars(steps []string) []string {
	seen := map[string]struct{}{}
	for _, s := range steps {
		for _, m := range regexp.MustCompile(`\$[1-9]\b`).FindAllString(s, -1) {
			seen[m] = struct{}{}
		}
		for _, m := range bracedVarRE.FindAllString(s, -1) {
			seen[m[2:len(m)-1]] = struct{}{}
		}
		for _, m := range bareVarRE.FindAllString(s, -1) {
			seen[m[1:]] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func validVarName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if i == 0 && !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return false
		}
		if i > 0 && !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
