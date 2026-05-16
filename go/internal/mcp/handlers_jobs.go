package mcp

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/prune"
	"srv/internal/remote"
	"strconv"
	"strings"
	"time"
)

// listJobView is the compacted job shape `handleListJobs` emits. The
// raw jobs.Record can carry several-hundred-char command bodies
// (think: the big system-probe scripts agents like to one-shot);
// echoing them verbatim in list_jobs makes the response grow
// linearly with history × cmd-length. We truncate cmd here to keep
// the per-record cost bounded, and add a Finished field so the
// model can distinguish historical entries (which 2.6.7+ retains
// after wait_job / kill_job) from still-running ones.
type listJobView struct {
	ID       string `json:"id"`
	Profile  string `json:"profile"`
	Cmd      string `json:"cmd"`
	Cwd      string `json:"cwd,omitempty"`
	Pid      int    `json:"pid"`
	Log      string `json:"log,omitempty"`
	Started  string `json:"started,omitempty"`
	Finished string `json:"finished,omitempty"`
	Killed   bool   `json:"killed,omitempty"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// listJobsCmdMax bounds the cmd field width in list_jobs responses.
// 80 chars is wide enough for typical commands; longer scripts get
// truncated with "... [+N chars]" so the model knows there's more
// in the raw record if it really wants it (via list_jobs filtered
// down to the one id, where the full cmd is acceptable token cost).
const listJobsCmdMax = 80

// reconcileFinished heals the ledger: for every still-"running"
// record it probes that profile's ~/.srv-jobs/*.exit markers (one
// round-trip per profile) and, when the marker exists, records the
// real Finished time + exit code. Returns true if any record changed
// so the caller can persist once. Profiles missing from cfg or
// unreachable are left untouched (treated as still-running).
func reconcileFinished(rs []*jobs.Record, cfg *config.Config) bool {
	byProf := map[string][]*jobs.Record{}
	for _, j := range rs {
		if j.Finished == "" {
			byProf[j.Profile] = append(byProf[j.Profile], j)
		}
	}
	if len(byProf) == 0 {
		return false
	}
	// Portable across sh/bash/zsh; the `ls | grep` guard avoids a
	// zsh "no matches found" abort when there are no .exit files yet.
	const probe = `cd ~/.srv-jobs 2>/dev/null || exit 0; for f in $(ls -1 2>/dev/null | grep '\.exit$'); do printf '%s %s\n' "${f%.exit}" "$(cat "$f" 2>/dev/null)"; done`
	changed := false
	for profName, recs := range byProf {
		prof, ok := cfg.Profiles[profName]
		if !ok {
			continue
		}
		res, err := remote.RunCapture(prof, "", probe)
		if err != nil || res == nil {
			continue
		}
		codes := map[string]int{}
		for _, line := range strings.Split(res.Stdout, "\n") {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			code := 0
			if len(f) > 1 {
				code, _ = strconv.Atoi(f[1])
			}
			codes[f[0]] = code
		}
		now := time.Now().Format(time.RFC3339)
		for _, j := range recs {
			if c, present := codes[j.ID]; present {
				j.Finished = now
				cc := c
				j.ExitCode = &cc
				changed = true
			}
		}
	}
	return changed
}

func handleListJobs(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	jf := jobs.Load()
	// Reconcile the WHOLE ledger against the remote .exit markers
	// before reporting. Without this a finished job stayed "running"
	// in the ledger forever (only a targeted wait_job ever reconciled
	// it), so list_jobs was untrustworthy and never self-healed.
	// Reconcile all jobs (not just the filtered view) so the on-disk
	// ledger heals globally; persist once if anything changed.
	if reconcileFinished(jf.Jobs, cfg) {
		_ = jobs.Save(jf)
	}
	rs := jf.Jobs
	if profileOverride != "" {
		filtered := make([]*jobs.Record, 0, len(rs))
		for _, j := range rs {
			if j.Profile == profileOverride {
				filtered = append(filtered, j)
			}
		}
		rs = filtered
	}
	views := make([]listJobView, 0, len(rs))
	for _, j := range rs {
		v := listJobView{
			ID:       j.ID,
			Profile:  j.Profile,
			Cwd:      j.Cwd,
			Pid:      j.Pid,
			Log:      j.Log,
			Started:  j.Started,
			Finished: j.Finished,
			Killed:   j.Killed,
			ExitCode: j.ExitCode,
		}
		v.Cmd = j.Cmd
		if len(v.Cmd) > listJobsCmdMax {
			over := len(v.Cmd) - listJobsCmdMax
			v.Cmd = v.Cmd[:listJobsCmdMax] + fmt.Sprintf(" ... [+%d chars]", over)
		}
		views = append(views, v)
	}
	return jsonResult(map[string]any{"jobs": views})
}

func handleTailLog(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	lines := 200
	if v, ok := args["lines"].(float64); ok {
		lines = int(v)
	}
	jf := jobs.Load()
	j, err := jobs.Resolve(jf, jid)
	if err != nil {
		return textErr(err.Error())
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return textErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	res, _ := remote.RunCapture(prof, "", fmt.Sprintf("tail -n %d %s", lines, j.Log))
	text := res.Stdout
	if text == "" {
		text = res.Stderr
	}
	if len(text) > ResultByteMax {
		return oversizeResult("tail_log", len(text),
			"lower `lines`, or use `run \"grep PATTERN ~/.srv-jobs/<id>.log | head -n N\"` to filter the job log directly",
			map[string]any{"job_id": j.ID, "exit_code": res.ExitCode})
	}
	// Content-only: the log IS the payload; a {job_id, exit_code}
	// structured stub would make the client hide it on success.
	return payloadResult(text, res.ExitCode != 0)
}

// classifyWaitStatus parses the poll script's stdout into (status,
// exitCode, body). The script's contract: its first line is exactly
// one of `STATUS=completed EXIT=<n>`, `STATUS=killed`, or
// `STATUS=running` (the last is its unconditional fallthrough), and
// everything after the first newline is the log-tail body.
//
// status == "unknown" is reserved for output matching NONE of those
// -- empty stdout (a transport / gzip-wrapper glitch) or unexpected
// shell noise. The script cannot legitimately produce that, so
// handleWaitJob turns it into a retryable error rather than a
// successful poll. exitCode is -1 unless a STATUS=completed EXIT=<n>
// was parsed.
func classifyWaitStatus(stdout string) (status string, exitCode int, body string) {
	statusLine, body, _ := strings.Cut(stdout, "\n")
	exitCode = -1
	switch {
	case strings.HasPrefix(statusLine, "STATUS=completed"):
		status = "completed"
		if _, after, ok := strings.Cut(statusLine, "EXIT="); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(after)); err == nil {
				exitCode = n
			}
		}
	case strings.HasPrefix(statusLine, "STATUS=killed"):
		status = "killed"
	case strings.HasPrefix(statusLine, "STATUS=running"):
		status = "running"
	default:
		status = "unknown"
	}
	return status, exitCode, body
}

func handleWaitJob(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	maxWait := waitJobDefaultSeconds
	if v, ok := args["max_wait_seconds"].(float64); ok && v > 0 {
		maxWait = int(v)
	}
	if maxWait > waitJobMaxSeconds {
		maxWait = waitJobMaxSeconds
	}
	tailLines := 50
	if v, ok := args["tail_lines"].(float64); ok && v > 0 {
		tailLines = int(v)
	}
	jf := jobs.Load()
	j, err := jobs.Resolve(jf, jid)
	if err != nil {
		return textErr(err.Error())
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return textErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	// One remote round-trip drives the whole wait loop. Bash spins
	// for up to maxWait seconds, checking each second for either
	// the .exit marker (job finished, capture exit code) or the
	// PID being gone without an .exit (got killed externally).
	// Either resolution prints `STATUS=...` on the first line plus
	// the log tail; if maxWait elapses the same shape is returned
	// with STATUS=running so the model can loop.
	exitFile := fmt.Sprintf("~/.srv-jobs/%s.exit", j.ID)
	script := fmt.Sprintf(`for i in $(seq 1 %d); do
  if [ -f %s ]; then
    code=$(cat %s)
    printf 'STATUS=completed EXIT=%%s\n' "$code"
    tail -n %d %s
    exit 0
  fi
  if ! kill -0 %d 2>/dev/null; then
    echo STATUS=killed
    tail -n %d %s
    exit 0
  fi
  sleep 1
done
echo STATUS=running
tail -n %d %s
`, maxWait, exitFile, exitFile, tailLines, j.Log, j.Pid, tailLines, j.Log, tailLines, j.Log)
	start := time.Now()
	res, _ := remote.RunCapture(prof, "", script)
	waited := time.Since(start).Seconds()

	status, exitCode, body := classifyWaitStatus(res.Stdout)
	switch status {
	case "completed":
		// Job finished -- record the outcome but KEEP the entry so
		// follow-up tail_log calls can still surface the historical
		// log. Earlier versions pruned the row here, which caused
		// "no such job" on every post-completion tail_log even
		// though the .log file was right where it had always been.
		// Explicit cleanup is now `srv prune jobs` (CLI) or
		// kill_job (still prunes, since the user is explicitly
		// asking to discard).
		j.Finished = time.Now().Format(time.RFC3339)
		ec := exitCode
		j.ExitCode = &ec
		_ = jobs.Save(jf)
	case "killed":
		// External kill detected -- mark so subsequent list_jobs /
		// tail_log calls can distinguish from a clean completion.
		j.Finished = time.Now().Format(time.RFC3339)
		j.Killed = true
		_ = jobs.Save(jf)
	}

	// The poll script always emits exactly one of STATUS=completed /
	// killed / running (its final line is an unconditional `echo
	// STATUS=running`). A status that is still "unknown" here means
	// the remote produced no recognizable marker -- empty stdout from
	// a transport/wrapper glitch, or unexpected shell noise. Handing
	// that back as a non-error success gives the model an actionless
	// {status:"unknown"} it cannot poll on (and it would wrongly look
	// done). Surface it as an error with whatever diagnostics we have
	// so the polling loop retries instead of silently giving up. Job
	// state is deliberately NOT mutated -- we don't know the outcome.
	if status == "unknown" {
		diag := strings.TrimSpace(res.Stderr)
		if diag == "" {
			snippet := res.Stdout
			if len(snippet) > 120 {
				snippet = snippet[:120]
			}
			diag = fmt.Sprintf("empty/unrecognized poll output (exit %d, stdout %q)", res.ExitCode, snippet)
		}
		return textErr(fmt.Sprintf(
			"wait_job: could not determine status of %q after %.1fs -- %s. The job may still be running; call wait_job again.",
			j.ID, waited, diag))
	}

	var hint string
	switch status {
	case "completed":
		hint = fmt.Sprintf("[%s exit=%d after %.1fs]", status, exitCode, waited)
	case "running":
		hint = fmt.Sprintf("[%s after %.1fs -- call wait_job again to keep waiting, or kill_job to stop]", status, waited)
	default:
		hint = fmt.Sprintf("[%s after %.1fs]", status, waited)
	}
	text := hint
	if body != "" {
		text += "\n" + body
	}
	structured := map[string]any{
		"job_id":         j.ID,
		"status":         status,
		"waited_seconds": waited,
	}
	if status == "completed" {
		structured["exit_code"] = exitCode
	}
	// Log tail can exceed the cap on chatty jobs even at default
	// tail_lines=50. Status/exit_code still flow back via
	// structuredContent so the polling loop can advance, but the
	// body text is rejected and the caller is told how to fetch
	// less. IsError reflects the actual JOB outcome (killed or
	// non-zero exit), not the size-rejection itself -- a successful
	// job with a chatty log shouldn't look like a tool failure to
	// the MCP client's UI.
	if len(text) > ResultByteMax {
		r := oversizeResult("wait_job", len(text),
			"lower `tail_lines`, or fetch the log separately with `tail_log` + a smaller `lines`",
			structured)
		r.IsError = status == "killed" || (status == "completed" && exitCode != 0)
		return r
	}
	// Content-only on the non-oversize path: the `hint` line already
	// encodes status + exit + waited in a form the polling loop reads
	// ("[running after 8.0s -- call wait_job again ...]"), and `body`
	// carries the log tail. A structured stub here would make the
	// client hide both behind {status, exit_code}. The oversize
	// branch above keeps `structured` precisely because there the
	// body is rejected and the loop has only the stub to advance on.
	return payloadResult(text, status == "killed" || (status == "completed" && exitCode != 0))
}

func handleKillJob(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	sig, _ := args["signal"].(string)
	if sig == "" {
		sig = "TERM"
	}
	if !isSafeSignal(sig) {
		return textErr(fmt.Sprintf("invalid signal %q (use a name like TERM/KILL/USR1 or a number)", sig))
	}
	jf := jobs.Load()
	j, err := jobs.Resolve(jf, jid)
	if err != nil {
		return textErr(err.Error())
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return textErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	// Signal the whole process GROUP (`kill -SIG -PID`), not just the
	// recorded pid. detach now spawns under setsid, so the recorded
	// pid is the group leader and -PID reaches the real workload
	// (e.g. the `sleep` child) -- previously kill hit only the bash
	// wrapper and orphaned the child while reporting a false "killed".
	// Fall back to the bare pid for pre-setsid (legacy) jobs. Check
	// the .exit marker first so an already-finished job reports its
	// real outcome instead of a misleading "no such pid".
	exitf := fmt.Sprintf("$HOME/.srv-jobs/%s.exit", j.ID)
	cmd := fmt.Sprintf(`if [ -f %s ]; then printf 'ALREADY_FINISHED %%s\n' "$(cat %s 2>/dev/null)"; elif kill -%s -%d 2>/dev/null || kill -%s %d 2>/dev/null; then echo SIGNALLED; else echo NO_PROCESS; fi`,
		exitf, exitf, sig, j.Pid, sig, j.Pid)
	res, _ := remote.RunCapture(prof, "", cmd)
	out := strings.TrimSpace(res.Stdout)
	tok := out
	if i := strings.IndexByte(tok, ' '); i >= 0 {
		tok = tok[:i]
	}
	var text string
	switch tok {
	case "ALREADY_FINISHED":
		code := 0
		if f := strings.Fields(out); len(f) > 1 {
			code, _ = strconv.Atoi(f[1])
		}
		j.Finished = time.Now().Format(time.RFC3339)
		ec := code
		j.ExitCode = &ec
		_ = jobs.Save(jf)
		text = fmt.Sprintf("job %s already finished (exit %d); nothing to kill", j.ID, code)
	case "SIGNALLED":
		j.Finished = time.Now().Format(time.RFC3339)
		j.Killed = true
		_ = jobs.Save(jf)
		text = fmt.Sprintf("killed: signal %s sent to job %s process group", sig, j.ID)
	case "NO_PROCESS":
		j.Finished = time.Now().Format(time.RFC3339)
		j.Killed = true
		_ = jobs.Save(jf)
		text = fmt.Sprintf("no live process for job %s (already exited; no exit marker recorded)", j.ID)
	default:
		text = out
		if text == "" {
			text = strings.TrimSpace(res.Stderr)
		}
		if text == "" {
			text = "kill_job: unrecognized result from remote"
		}
		return textErr(text)
	}
	return payloadResult(text, false)
}

// isSafeSignal guards the signal that gets interpolated into the
// remote `kill -%s` -- only a bare name (TERM, SIGUSR1) or number
// (9, 15). Anything else could inject shell.
func isSafeSignal(s string) bool {
	if s == "" || len(s) > 12 {
		return false
	}
	for _, r := range s {
		if !((r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// handlePruneJobs is the AI's "receipt": after it has consumed a
// job's result (wait_job / tail_log) it calls this to discard the
// finished record so list_jobs stays small and trustworthy. It is
// the MCP counterpart of `srv prune jobs`, sharing prune.PruneLedger
// so the keep-live semantics never drift: still-running jobs are
// always kept; an `id` that is still running is refused (we'd orphan
// the remote pid). Reconciles against remote .exit markers first so
// a job that finished since the ledger was last touched is pruned by
// this same call instead of lingering as a stale "running" row.
func handlePruneJobs(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	id, _ := args["id"].(string)
	jf := jobs.Load()
	reconcileFinished(jf.Jobs, cfg)
	n, err := prune.PruneLedger(jf, id)
	if err != nil {
		return textErr(err.Error())
	}
	if err := jobs.Save(jf); err != nil {
		return textErr(err.Error())
	}
	var msg string
	switch {
	case id != "":
		msg = fmt.Sprintf("pruned finished job %q; %d record(s) remain", id, len(jf.Jobs))
	case n == 0:
		msg = "no finished job records to prune"
	default:
		msg = fmt.Sprintf("pruned %d finished job record(s); %d remain (running jobs kept)", n, len(jf.Jobs))
	}
	return jsonResult(map[string]any{
		"pruned":    n,
		"remaining": len(jf.Jobs),
		"message":   msg,
	})
}
