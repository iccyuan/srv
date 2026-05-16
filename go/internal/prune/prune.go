// Package prune implements `srv prune <target> [--remote]` -- the one
// place that clears srv's accumulated local caches and history, plus
// (opt-in) the remote per-job log/exit files under ~/.srv-jobs/.
//
// It supersedes the old `srv jobs prune`: a single top-level verb with
// a tab-completable target enum is less confusing than a prune action
// buried under one noun while sessions had its own. Targets:
//
//	jobs        finished records in the local jobs.json ledger
//	sessions    session.json rows whose pid is dead
//	mcp-log     mcp.log trimmed to its recent tail (256 KB)
//	mcp-stats   mcp-stats.jsonl rows newer than the retention window
//	all         every local cache above, in one pass
//
// Every target is a true prune, not a wipe: it drops the stale part
// and keeps the live/recent part. jobs keeps running jobs, sessions
// keeps live-pid rows, mcp-log keeps the recent tail, mcp-stats keeps
// the last StatsKeepWindow of telemetry. Full erasure is a different
// verb (e.g. `srv stats --clear` / `srv sessions clear`) on purpose.
//
// --remote is an explicit, never-implied opt-in (it deletes data on a
// production server). It applies only to the jobs target (and to `all`,
// which routes its job step through here): it removes ~/.srv-jobs/
// *.log + *.exit for COMPLETED jobs on the resolved profile's host. A
// job is "completed" iff its <id>.exit marker exists remotely, so a
// still-running job (log present, no .exit yet) is never touched -- the
// same invariant the local ledger prune has always held.
package prune

import (
	"fmt"
	"strings"
	"time"

	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/mcplog"
	"srv/internal/mcpstats"
	"srv/internal/remote"
	"srv/internal/session"
	"srv/internal/srvutil"
)

// StatsKeepWindow is how much recent MCP telemetry `srv prune
// mcp-stats` (and `all`) retains; older rows are dropped. Seven days
// is long enough to span a working week's `srv stats` review yet short
// enough that the file doesn't creep back toward its 10 MiB rotation
// cap. Named + exported so help text and any future --days override
// read one source.
const StatsKeepWindow = 7 * 24 * time.Hour

// Targets is the ordered, user-facing target list. The completion DSL
// (internal/completion/spec.go) and the prose help read the same set so
// the three never drift.
var Targets = []string{"jobs", "sessions", "mcp-log", "mcp-stats", "all"}

// Cmd is the `srv prune` entry point. args[0] is the target; --remote
// may appear anywhere. A bare `srv prune` prints usage rather than
// guessing -- prune is destructive enough that a no-op-on-empty would
// hide typos.
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	target := ""
	remoteSweep := false
	rest := []string{}
	for _, a := range args {
		switch {
		case a == "--remote":
			remoteSweep = true
		case target == "" && !strings.HasPrefix(a, "-"):
			target = a
		default:
			rest = append(rest, a)
		}
	}
	if target == "" {
		return usage()
	}

	switch target {
	case "jobs":
		return pruneJobs(rest, cfg, profileOverride, remoteSweep)
	case "sessions":
		return pruneSessions()
	case "mcp-log":
		return pruneMCPLog()
	case "mcp-stats":
		return pruneMCPStats()
	case "all":
		return pruneAll(cfg, profileOverride, remoteSweep)
	}
	return clierr.Errf(2, "unknown prune target %q (try: %s)", target, strings.Join(Targets, ", "))
}

func usage() error {
	return clierr.Errf(2, `usage: srv prune <target> [--remote]

every target keeps the live/recent part and drops only the stale part
(full erasure is a different verb -- e.g. srv stats --clear):
  jobs        drop FINISHED job records (running jobs kept)
  sessions    drop DEAD-pid session records (live sessions kept)
  mcp-log     trim mcp.log to its recent ~256 KB tail
  mcp-stats   drop telemetry rows older than 7d (recent kept)
  all         every prune above, in one pass

--remote   jobs/all only: also delete COMPLETED jobs' ~/.srv-jobs/*.log
           and *.exit on the resolved profile's server. Running jobs are
           never touched. Off unless this flag is given.`)
}

// pruneJobs handles the local ledger sweep (the old `srv jobs prune`
// behaviour, verbatim semantics) and, when remoteSweep is set, the
// server-side ~/.srv-jobs/ cleanup. rest[0], if present, narrows both
// to a single job id.
func pruneJobs(rest []string, cfg *config.Config, profileOverride string, remoteSweep bool) error {
	jf := jobs.Load()
	if jf == nil {
		jf = &jobs.File{}
	}
	target := ""
	if len(rest) > 0 {
		target = rest[0]
	}

	kept := jf.Jobs[:0]
	pruned := 0
	for _, j := range jf.Jobs {
		switch {
		case target != "" && j.ID != target:
			kept = append(kept, j)
		case target != "" && j.ID == target:
			// Refuse to drop a still-running job by id: losing the
			// local record orphans the remote pid + log path.
			if j.Finished == "" {
				return clierr.Errf(1, "job %q is still running; use `srv kill %s` first", target, target)
			}
			pruned++
		case target == "" && j.Finished != "":
			pruned++
		default:
			kept = append(kept, j)
		}
	}
	jf.Jobs = kept
	if err := jobs.Save(jf); err != nil {
		return clierr.Errf(1, "save: %v", err)
	}
	if pruned == 0 {
		fmt.Println("(no finished job records to prune)")
	} else {
		fmt.Printf("pruned %d finished job record(s)\n", pruned)
	}

	if !remoteSweep {
		return nil
	}
	return sweepRemoteJobs(target, cfg, profileOverride)
}

// sweepRemoteJobs deletes the log/exit pair for completed jobs under
// ~/.srv-jobs/ on the resolved profile's host. When target != "" only
// that job's files go (it was already verified finished by pruneJobs);
// otherwise every job carrying a .exit marker -- which by construction
// excludes still-running jobs -- is removed in one round-trip.
func sweepRemoteJobs(target string, cfg *config.Config, profileOverride string) error {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "--remote: %v", err)
	}
	var sh string
	if target != "" {
		sh = fmt.Sprintf(`rm -f ~/.srv-jobs/%s.log ~/.srv-jobs/%s.exit && echo 1 || echo 0`, target, target)
	} else {
		// Literal-glob safe: with no matches the loop body runs once
		// with e="*.exit", the `-e` test fails, and we break with n=0.
		sh = `n=0; cd ~/.srv-jobs 2>/dev/null || { echo 0; exit 0; }; ` +
			`for e in *.exit; do [ -e "$e" ] || break; id="${e%.exit}"; ` +
			`rm -f "$id.exit" "$id.log" && n=$((n+1)); done; echo "$n"`
	}
	res, err := remote.RunCapture(profile, "", sh)
	if err != nil {
		return clierr.Errf(1, "--remote sweep on %q failed: %v", name, err)
	}
	cnt := strings.TrimSpace(res.Stdout)
	if cnt == "" {
		cnt = "0"
	}
	fmt.Printf("remote: deleted %s completed job log/exit pair(s) on %s (~/.srv-jobs/)\n", cnt, name)
	return nil
}

func pruneSessions() error {
	removed, before := session.PruneDead(srvutil.PidAlive)
	fmt.Printf("pruned %d/%d sessions (%d remaining)\n", removed, before, before-removed)
	return nil
}

// pruneMCPLog trims mcp.log to its recent tail, keeping the latest
// ~256 KB of diagnostics and dropping the older head. Absent/within-
// retention is success (prune is re-run without checking first).
func pruneMCPLog() error {
	kept, dropped, err := mcplog.Prune()
	if err != nil {
		return clierr.Errf(1, "mcp.log: %v", err)
	}
	if dropped == 0 {
		fmt.Printf("(mcp.log within retention; nothing to prune)\n")
		return nil
	}
	fmt.Printf("pruned mcp.log (kept %d B, dropped %d B)\n", kept, dropped)
	return nil
}

// pruneMCPStats drops telemetry rows older than StatsKeepWindow,
// keeping recent ones (and the rotated .1 sibling gets the same
// cut). Nothing old enough is success, not a no-op-worthy error.
func pruneMCPStats() error {
	cutoff := time.Now().Add(-StatsKeepWindow)
	kept, dropped, err := mcpstats.PruneOlderThan(cutoff)
	if err != nil {
		return clierr.Errf(1, "mcp-stats.jsonl: %v", err)
	}
	if dropped == 0 {
		fmt.Printf("(mcp-stats.jsonl: nothing older than %s)\n", StatsKeepWindow)
		return nil
	}
	fmt.Printf("pruned mcp-stats.jsonl (kept %d row(s), dropped %d older than %s)\n",
		kept, dropped, StatsKeepWindow)
	return nil
}

// pruneAll runs every local cache prune in one pass. The jobs step is
// routed through pruneJobs so a single `srv prune all --remote` also
// performs the server-side ~/.srv-jobs/ sweep exactly once.
func pruneAll(cfg *config.Config, profileOverride string, remoteSweep bool) error {
	if err := pruneJobs(nil, cfg, profileOverride, remoteSweep); err != nil {
		return err
	}
	if err := pruneSessions(); err != nil {
		return err
	}
	if err := pruneMCPLog(); err != nil {
		return err
	}
	return pruneMCPStats()
}
