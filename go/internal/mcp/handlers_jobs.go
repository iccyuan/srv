package mcp

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/jobs"
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

func handleListJobs(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	rs := jobs.Load().Jobs
	if profileOverride != "" {
		filtered := rs[:0]
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
	j := jobs.Find(jf, jid)
	if j == nil {
		return textErr(fmt.Sprintf("no such job %q", jid))
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
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           res.ExitCode != 0,
		StructuredContent: map[string]any{"job_id": j.ID, "exit_code": res.ExitCode},
	}
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
	j := jobs.Find(jf, jid)
	if j == nil {
		return textErr(fmt.Sprintf("no such job %q", jid))
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

	lines := strings.SplitN(res.Stdout, "\n", 2)
	statusLine := ""
	body := ""
	if len(lines) > 0 {
		statusLine = lines[0]
	}
	if len(lines) > 1 {
		body = lines[1]
	}
	status := "unknown"
	exitCode := -1
	if strings.HasPrefix(statusLine, "STATUS=completed") {
		status = "completed"
		if i := strings.Index(statusLine, "EXIT="); i >= 0 {
			if n, err := strconv.Atoi(strings.TrimSpace(statusLine[i+5:])); err == nil {
				exitCode = n
			}
		}
		// Job finished -- record the outcome but KEEP the entry so
		// follow-up tail_log calls can still surface the historical
		// log. Earlier versions pruned the row here, which caused
		// "no such job" on every post-completion tail_log even
		// though the .log file was right where it had always been.
		// Explicit cleanup is now `srv jobs prune` (CLI) or
		// kill_job (still prunes, since the user is explicitly
		// asking to discard).
		j.Finished = time.Now().Format(time.RFC3339)
		ec := exitCode
		j.ExitCode = &ec
		_ = jobs.Save(jf)
	} else if strings.HasPrefix(statusLine, "STATUS=killed") {
		status = "killed"
		// External kill detected -- mark so subsequent list_jobs /
		// tail_log calls can distinguish from a clean completion.
		j.Finished = time.Now().Format(time.RFC3339)
		j.Killed = true
		_ = jobs.Save(jf)
	} else if strings.HasPrefix(statusLine, "STATUS=running") {
		status = "running"
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
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           status == "killed" || (status == "completed" && exitCode != 0),
		StructuredContent: structured,
	}
}

func handleKillJob(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	jid, _ := args["id"].(string)
	sig, _ := args["signal"].(string)
	if sig == "" {
		sig = "TERM"
	}
	jf := jobs.Load()
	j := jobs.Find(jf, jid)
	if j == nil {
		return textErr(fmt.Sprintf("no such job %q", jid))
	}
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return textErr(fmt.Sprintf("profile %q not found", j.Profile))
	}
	cmd := fmt.Sprintf("kill -%s %d 2>/dev/null && echo killed || echo 'no such pid'", sig, j.Pid)
	res, _ := remote.RunCapture(prof, "", cmd)
	// Mark the job as killed but keep the record so tail_log still
	// works for post-mortem inspection. The CLI `srv kill` is the
	// place users go for explicit cleanup; MCP's kill_job is more
	// often "please stop this, I'll look at the log after."
	j.Finished = time.Now().Format(time.RFC3339)
	j.Killed = true
	_ = jobs.Save(jf)
	text := strings.TrimSpace(res.Stdout)
	if text == "" {
		text = strings.TrimSpace(res.Stderr)
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           res.ExitCode != 0,
		StructuredContent: map[string]any{"job_id": j.ID, "signal": sig, "exit_code": res.ExitCode},
	}
}
