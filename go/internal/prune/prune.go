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

// PruneLedger drops FINISHED records from jf in place, keeping every
// still-running one. If target != "" only that id is dropped and it
// must be finished (running -> error, so we never orphan a live
// remote pid + log path). Returns how many were pruned. The caller
// persists + reports.
//
// This is the shared keep-live core for `srv prune jobs` (CLI) and
// the prune_jobs MCP tool so their semantics never drift -- the same
// single-source discipline the package doc describes. A fresh slice
// (not jf.Jobs[:0]) is built so an early error return can't leave a
// half-rewritten ledger in a long-lived process (the MCP server).
func PruneLedger(jf *jobs.File, target string) (int, error) {
	kept := make([]*jobs.Record, 0, len(jf.Jobs))
	pruned := 0
	for _, j := range jf.Jobs {
		switch {
		case target != "" && j.ID != target:
			kept = append(kept, j)
		case target != "" && j.ID == target:
			if j.Finished == "" {
				return 0, fmt.Errorf("job %q is still running; kill it first", target)
			}
			pruned++
		case target == "" && j.Finished != "":
			pruned++
		default:
			kept = append(kept, j)
		}
	}
	jf.Jobs = kept
	return pruned, nil
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

	pruned, err := PruneLedger(jf, target)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}
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
// that job's files go; otherwise every job carrying a .exit marker --
// which by construction excludes still-running jobs -- is removed in
// one round-trip. Both paths gate on the .exit marker so a running job
// (log present, no .exit yet) is never touched -- see remoteSweepScript.
func sweepRemoteJobs(target string, cfg *config.Config, profileOverride string) error {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "--remote: %v", err)
	}
	res, err := remote.RunCapture(profile, "", remoteSweepScript(target))
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

// remoteSweepScript builds the POSIX sh deletion command. The cardinal
// invariant (also stated in the package doc): a job's files are removed
// only when its .exit marker exists, so a still-running job -- log
// present, no .exit yet -- is never touched. This must hold for BOTH
// paths, because the targeted path is reachable for an id that is NOT
// in the local ledger (stale/already-pruned ledger, an id from another
// machine, or a typo colliding with a live job's id): in that case
// pruneJobs prunes zero records and never verifies the job finished, so
// the gate cannot be delegated to it -- it lives here unconditionally.
//
//	target != ""  delete <id>.{log,exit} iff <id>.exit exists -> echo 1/0
//	target == ""  iterate *.exit only (literal-glob safe: with no
//	              matches the body runs once with e="*.exit", the -e
//	              test fails, and we break with n=0) -> echo count
func remoteSweepScript(target string) string {
	if target != "" {
		return fmt.Sprintf(
			`[ -e ~/.srv-jobs/%s.exit ] && `+
				`rm -f ~/.srv-jobs/%s.log ~/.srv-jobs/%s.exit && echo 1 || echo 0`,
			target, target, target)
	}
	return `n=0; cd ~/.srv-jobs 2>/dev/null || { echo 0; exit 0; }; ` +
		`for e in *.exit; do [ -e "$e" ] || break; id="${e%.exit}"; ` +
		`rm -f "$id.exit" "$id.log" && n=$((n+1)); done; echo "$n"`
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
