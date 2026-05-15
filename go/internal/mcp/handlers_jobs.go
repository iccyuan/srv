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

func handleListJobs(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	rs := jobs.Load().Jobs
	if profileOverride != "" {
		out := rs[:0]
		for _, j := range rs {
			if j.Profile == profileOverride {
				out = append(out, j)
			}
		}
		rs = out
	}
	return jsonResult(map[string]any{"jobs": rs})
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
