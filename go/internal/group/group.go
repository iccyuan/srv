package group

import (
	"encoding/json"
	"fmt"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/remote"
	"strings"
	"sync"
	"time"
)

// Profile groups let users tag a set of profiles by name (e.g. all
// web frontends, all staging boxes) and run one command against every
// member in parallel. This is the killer ops use case: "restart nginx
// on every web host" becomes one command, not a for-loop.
//
// On-disk shape lives in Config.Groups (config.go); CLI surface is the
// `srv group <list|show|set|remove>` subcommand plus the `-G <group>`
// global flag that swaps the per-profile run for a fan-out run.

// Cmd is the `srv group <action>` dispatcher. Kept tiny because
// each branch is one screen of code; promoting to subcommands of its
// own felt heavier than the feature warranted.
func Cmd(args []string, cfg *config.Config) error {
	if len(args) == 0 {
		return listCmd(cfg)
	}
	switch args[0] {
	case "list", "ls":
		return listCmd(cfg)
	case "show":
		if len(args) < 2 {
			return clierr.Errf(2, "usage: srv group show <name>")
		}
		return showCmd(cfg, args[1])
	case "set":
		if len(args) < 3 {
			return clierr.Errf(2, "usage: srv group set <name> <profile> [profile...]")
		}
		return setCmd(cfg, args[1], args[2:])
	case "remove", "rm":
		if len(args) < 2 {
			return clierr.Errf(2, "usage: srv group remove <name>")
		}
		return removeCmd(cfg, args[1])
	default:
		return clierr.Errf(2, "unknown group action %q (expected list/show/set/remove)", args[0])
	}
}

func listCmd(cfg *config.Config) error {
	if len(cfg.Groups) == 0 {
		fmt.Println("(no groups defined)")
		return nil
	}
	names := make([]string, 0, len(cfg.Groups))
	for n := range cfg.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		members := cfg.Groups[n]
		fmt.Printf("%s (%d): %s\n", n, len(members), strings.Join(members, ", "))
	}
	return nil
}

func showCmd(cfg *config.Config, name string) error {
	members, ok := cfg.Groups[name]
	if !ok {
		return clierr.Errf(1, "group %q not found", name)
	}
	if len(members) == 0 {
		fmt.Printf("%s: (empty)\n", name)
		return nil
	}
	for _, m := range members {
		marker := ""
		if _, ok := cfg.Profiles[m]; !ok {
			marker = "  (NOT FOUND in profiles)"
		}
		fmt.Printf("%s%s\n", m, marker)
	}
	return nil
}

// setCmd replaces a group's membership wholesale. Replace-not-merge
// is the safer default: explicit add-member / remove-member ops would
// surface as two more subcommands without adding power -- the user can
// always re-run set with the new list. We do validate each member is a
// known profile up front so the saved config can never reference a
// ghost name.
func setCmd(cfg *config.Config, name string, members []string) error {
	if len(members) == 0 {
		return clierr.Errf(2, "set requires at least one member; use `srv group remove %s` to delete", name)
	}
	for _, m := range members {
		if _, ok := cfg.Profiles[m]; !ok {
			return clierr.Errf(1, "profile %q not found", m)
		}
	}
	// De-duplicate while preserving the order the user typed -- order
	// matters for "all-or-nothing" sequencing if we ever add a serial
	// mode, and a stable order also keeps the JSON diff clean.
	seen := map[string]bool{}
	deduped := members[:0]
	for _, m := range members {
		if seen[m] {
			continue
		}
		seen[m] = true
		deduped = append(deduped, m)
	}
	if cfg.Groups == nil {
		cfg.Groups = map[string][]string{}
	}
	cfg.Groups[name] = deduped
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Printf("group %q set to %d member(s): %s\n", name, len(deduped), strings.Join(deduped, ", "))
	return nil
}

func removeCmd(cfg *config.Config, name string) error {
	if _, ok := cfg.Groups[name]; !ok {
		return clierr.Errf(1, "group %q not found", name)
	}
	delete(cfg.Groups, name)
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Printf("group %q removed\n", name)
	return nil
}

// Result is the per-profile outcome of a fan-out call. Captured
// in full so the MCP wrapper can return structured content and the CLI
// can render a section-per-profile report.
type Result struct {
	Profile  string  `json:"profile"`
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout,omitempty"`
	Stderr   string  `json:"stderr,omitempty"`
	Duration float64 `json:"duration_seconds"`
	// Error captures dial / session-layer failures (couldn't even reach
	// the host) as distinct from non-zero exit codes (the command ran
	// but failed). MCP clients can branch on which case they hit.
	Error string `json:"error,omitempty"`
}

// Run executes `cmd` on every profile in `groupName` in parallel
// and returns a result per profile. Order matches Config.Groups[name]
// so reports are deterministic across runs.
//
// Validation is up-front: a missing group or a member pointing at a
// ghost profile returns an error without touching any connection. This
// matters for safety (a typo in the group list shouldn't half-execute)
// and for cost (no SSH handshake until we know all members resolve).
//
// We cap parallelism at len(members) -- one goroutine per profile.
// SSH multiplexing inside each Client handles serializing operations
// per host; the daemon pool reuses connections across calls. For
// groups of a few dozen hosts this is fine; if anyone defines a 500-
// host group we'll revisit with a worker-pool limit.
func Run(cfg *config.Config, groupName, cmd string) ([]Result, error) {
	members, ok := cfg.Groups[groupName]
	if !ok {
		return nil, fmt.Errorf("group %q not found", groupName)
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("group %q is empty", groupName)
	}
	for _, name := range members {
		if _, ok := cfg.Profiles[name]; !ok {
			return nil, fmt.Errorf("group %q references unknown profile %q", groupName, name)
		}
	}

	results := make([]Result, len(members))
	var wg sync.WaitGroup
	for i, name := range members {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			prof := cfg.Profiles[name]
			prof.Name = name
			cwd := config.GetCwd(name, prof)
			start := time.Now()
			res, err := remote.RunCapture(prof, cwd, cmd)
			dur := time.Since(start).Seconds()
			if err != nil {
				results[i] = Result{Profile: name, ExitCode: -1, Error: err.Error(), Duration: dur}
				return
			}
			results[i] = Result{
				Profile:  name,
				ExitCode: res.ExitCode,
				Stdout:   res.Stdout,
				Stderr:   res.Stderr,
				Duration: dur,
			}
		}(i, name)
	}
	wg.Wait()
	return results, nil
}

// RunCmd is the CLI entry point for `-G <group> <cmd>`. Renders
// results as one section per profile, with a summary line at the end.
// Exit code is the maximum non-zero across members so CI / shell
// pipelines can detect partial failure.
func RunCmd(args []string, cfg *config.Config, groupName string) error {
	if len(args) == 0 {
		return clierr.Errf(2, "usage: srv -G <group> <command>")
	}
	cmd := strings.Join(args, " ")
	results, err := Run(cfg, groupName, cmd)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}
	maxExit, failed := RenderResults(results)
	fmt.Printf("\n%d profile(s), %d succeeded, %d failed.\n", len(results), len(results)-failed, failed)
	return clierr.Code(maxExit)
}

// RenderResults prints one section per result and returns
// (max-exit-code, failures). Sections are separated by a header line
// so output is greppable and the per-profile boundaries are visible.
func RenderResults(results []Result) (int, int) {
	maxExit, failed := 0, 0
	for _, r := range results {
		fmt.Printf("=== %s [exit %d, %.1fs]", r.Profile, r.ExitCode, r.Duration)
		if r.Error != "" {
			fmt.Printf(" ERROR: %s", r.Error)
		}
		fmt.Println(" ===")
		if r.Stdout != "" {
			fmt.Print(r.Stdout)
			if !strings.HasSuffix(r.Stdout, "\n") {
				fmt.Println()
			}
		}
		if r.Stderr != "" {
			fmt.Print(r.Stderr)
			if !strings.HasSuffix(r.Stderr, "\n") {
				fmt.Println()
			}
		}
		if r.ExitCode != 0 || r.Error != "" {
			failed++
			if r.ExitCode > maxExit {
				maxExit = r.ExitCode
			} else if r.ExitCode < 0 && maxExit == 0 {
				// Dial / session failure -- surface as 255 (the SSH
				// convention for "connection problem") so callers can
				// tell it apart from a real exit-1 from the command.
				maxExit = 255
			}
		}
	}
	return maxExit, failed
}

// ResultsJSON renders a JSON array of results -- used by the MCP
// tool when it wants a structured payload alongside the text report.
func ResultsJSON(results []Result) string {
	b, _ := json.Marshal(results)
	return string(b)
}
