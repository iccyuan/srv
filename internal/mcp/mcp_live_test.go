//go:build live

// Live MCP integration suite. SAFETY-CRITICAL FILE -- an earlier
// ad-hoc live test was run ~900x in a loop and froze the machine.
// Three independent guards make a repeat impossible:
//
//  1. `//go:build live` -- excluded from `go test ./...` and CI
//     entirely; it is never even compiled without `-tags live`.
//  2. SRV_LIVE_PROFILE env gate -- skips with the tag but no profile.
//  3. Runtime containment -- a package mutex serializes everything,
//     every handler call has a hard wall-clock timeout (no 10-min
//     hangs), the server-side generator is self-capped (testdata/
//     livegen.sh watchdog), and a guaranteed cleanup force-kills
//     every spawned process GROUP and prunes exactly the job IDs we
//     created (never touches unrelated ledger history).
//
// Run it explicitly, against your own remote, never in a loop:
//
//	SRV_LIVE_PROFILE=美国备用 go test -tags live -run TestLive \
//	    -timeout 240s -v ./internal/mcp/
//
// Subtests are individually targetable, e.g.:
//
//	... -run TestLive/KillJobNoCollateral ...
//
// It calls the handler functions directly, so each `go test` run
// exercises the CURRENT source -- no `/mcp` reconnect / rebuild.
package mcp

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/remote"
)

//go:embed testdata/livegen.sh
var livegenScript string

// liveMu serializes the whole suite. Subtests never call t.Parallel,
// but this still defends against `-parallel` / external concurrency:
// two tests dialing + spawning on the same remote at once is exactly
// the stampede that caused the prior incident.
var liveMu sync.Mutex

// liveHarness owns one subtest's remote state and guarantees teardown.
type liveHarness struct {
	t       *testing.T
	cfg     *config.Config
	name    string
	prof    *config.Profile
	tracked []*jobs.Record // every job WE spawned -> force-killed + pruned
	tmpPath []string       // every data file WE created -> removed
}

// newLiveHarness applies guards 2+3 and registers teardown. Teardown
// order (t.Cleanup is LIFO): force-kill+prune runs FIRST while the
// mutex is still held, then the mutex unlocks LAST.
func newLiveHarness(t *testing.T) *liveHarness {
	t.Helper()
	profName := os.Getenv("SRV_LIVE_PROFILE")
	if profName == "" {
		t.Skip("set SRV_LIVE_PROFILE=<profile> (and build with -tags live) to run the live MCP suite")
	}
	cfg, err := config.Load()
	if err != nil || cfg == nil {
		t.Skipf("no config (%v); cannot run live suite", err)
	}
	name, prof, err := config.Resolve(cfg, profName)
	if err != nil {
		t.Skipf("profile %q not resolvable: %v", profName, err)
	}

	liveMu.Lock()
	h := &liveHarness{t: t, cfg: cfg, name: name, prof: prof}
	t.Cleanup(func() { liveMu.Unlock() }) // registered first -> runs last
	t.Cleanup(h.teardown)                 // registered last  -> runs first
	return h
}

// shq single-quotes a string for safe embedding in `sh -c '...'`.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sh runs a bounded remote command and returns trimmed stdout + exit
// code. Wrapped in `timeout` so a wedged remote can never block the
// suite even if RunCapture itself didn't bound it.
func (h *liveHarness) sh(cmd string) (string, int) {
	h.t.Helper()
	res, err := remote.RunCapture(h.prof, "", "timeout 20 sh -c "+shq(cmd))
	if err != nil || res == nil {
		h.t.Fatalf("remote sh failed: %v (cmd=%q)", err, cmd)
	}
	return strings.TrimSpace(res.Stdout), res.ExitCode
}

// spawn launches the self-capped livegen generator as a detached job.
func (h *liveHarness) spawn(mode, token, path string, warmup, count, interval, maxlife int) *jobs.Record {
	h.t.Helper()
	body := livegenScript + fmt.Sprintf("\nlivegen %s %s %s %d %d %d %d\n",
		mode, token, path, warmup, count, interval, maxlife)
	return h.track(body, path)
}

// spawnRaw launches an arbitrary BOUNDED command as a detached job.
// Callers must pass only self-terminating commands (e.g. `sleep 6`,
// `echo x`) -- there is no watchdog around a raw command, so an
// unbounded one would leak until the next teardown kills it.
func (h *liveHarness) spawnRaw(cmd string) *jobs.Record {
	h.t.Helper()
	return h.track(cmd, "")
}

func (h *liveHarness) track(body, path string) *jobs.Record {
	rec, err := remote.SpawnDetached(h.name, h.prof, body)
	if err != nil {
		h.t.Fatalf("SpawnDetached: %v", err)
	}
	if rec == nil {
		h.t.Fatalf("SpawnDetached returned nil record")
	}
	h.tracked = append(h.tracked, rec)
	if path != "" && path != "/dev/null" {
		h.tmpPath = append(h.tmpPath, path)
	}
	return rec
}

// teardown is best-effort, bounded, and never panics. It force-kills
// each spawned process GROUP (-pid) and the bare pid, removes our
// remote files, and prunes ONLY our job IDs from the local ledger --
// unrelated history (the kind the prior cleanup nearly destroyed) is
// left strictly untouched.
func (h *liveHarness) teardown() {
	for _, r := range h.tracked {
		cmd := fmt.Sprintf(
			"kill -KILL -%d 2>/dev/null; kill -KILL %d 2>/dev/null; rm -f ~/.srv-jobs/%s.log ~/.srv-jobs/%s.exit",
			r.Pid, r.Pid, r.ID, r.ID)
		_, _ = remote.RunCapture(h.prof, "", "timeout 15 sh -c "+shq(cmd))
	}
	for _, p := range h.tmpPath {
		_, _ = remote.RunCapture(h.prof, "", "timeout 15 sh -c "+shq("rm -f "+p))
	}
	mine := map[string]bool{}
	for _, r := range h.tracked {
		mine[r.ID] = true
	}
	jf := jobs.Load()
	kept := jf.Jobs[:0]
	for _, j := range jf.Jobs {
		if !mine[j.ID] {
			kept = append(kept, j)
		}
	}
	jf.Jobs = kept
	_ = jobs.Save(jf)
}

// call runs a handler under a HARD wall-clock deadline. A blocked
// handler fails the test fast instead of hanging for `go test`'s
// 10-minute default -- the precise failure mode behind the incident.
func (h *liveHarness) call(label string, d time.Duration, fn func() toolResult) toolResult {
	h.t.Helper()
	ch := make(chan toolResult, 1) // buffered: a late goroutine won't block forever
	go func() { ch <- fn() }()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		h.t.Fatalf("%s exceeded hard timeout %s -- handler blocked; aborting", label, d)
		return toolResult{}
	}
}

func resText(r toolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// structMap extracts a tool result's StructuredContent as a string
// map. oversizeResult always attaches one (rejected_reason / cap_bytes
// / bytes_returned); nil here means the assertion fails loudly instead
// of a nil-map panic.
func structMap(r toolResult) map[string]any {
	m, _ := r.StructuredContent.(map[string]any)
	return m
}

// waitFor polls until check() is true. Bounded by BOTH a wall-clock
// deadline AND a fixed iteration cap, so it can never become the
// infinite loop that this whole file exists to prevent.
func (h *liveHarness) waitFor(what string, max time.Duration, check func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(max)
	for i := 0; i < 600; i++ {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s after %s", what, max)
}

// alive reports whether the job's recorded pid is still running and
// no .exit marker was written (i.e. it neither finished nor was
// killed). Used to prove kill isolation.
func (h *liveHarness) alive(r *jobs.Record) bool {
	out, _ := h.sh(fmt.Sprintf(
		"if [ -f ~/.srv-jobs/%s.exit ]; then echo EXITED; elif kill -0 %d 2>/dev/null; then echo ALIVE; else echo DEAD; fi",
		r.ID, r.Pid))
	return out == "ALIVE"
}

func uniq(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano())
}

func (h *liveHarness) tmp(tag string) string {
	return fmt.Sprintf("/tmp/srv-live-%s-%d-%d.log", tag, os.Getpid(), time.Now().UnixNano())
}

// TestLive groups every live scenario so the gate, the serialization
// mutex, and per-subtest teardown are centralized. Each subtest is
// independently runnable via -run TestLive/<Name>.
func TestLive(t *testing.T) {
	// --- one-shot tail: can we fetch exactly the data we asked for? ---
	t.Run("TailOneShot", func(t *testing.T) {
		h := newLiveHarness(t)
		tok := uniq("ONESHOT")
		path := h.tmp("oneshot")
		h.spawn("lines", tok, path, 0, 5, 0, 30) // 5 lines, instant

		h.waitFor("5 generated lines", 15*time.Second, func() bool {
			out, _ := h.sh(fmt.Sprintf("grep -c %s %s 2>/dev/null || echo 0", shq(tok), path))
			return out == "5"
		})

		r := h.call("tail one-shot", 15*time.Second, func() toolResult {
			return handleTail(map[string]any{
				"path": path, "lines": float64(50),
			}, h.cfg, h.name)
		})
		if r.IsError {
			t.Fatalf("tail one-shot errored: %s", resText(r))
		}
		txt := resText(r)
		prev := -1
		for i := 1; i <= 5; i++ {
			marker := fmt.Sprintf("%s %d ", tok, i)
			idx := strings.Index(txt, marker)
			if idx < 0 {
				t.Fatalf("missing %q in one-shot tail:\n%s", marker, txt)
			}
			if idx <= prev {
				t.Errorf("%q out of order (idx %d <= prev %d)", marker, idx, prev)
			}
			prev = idx
		}

		// grep filter must narrow server-fetched data correctly.
		rg := h.call("tail one-shot grep", 15*time.Second, func() toolResult {
			return handleTail(map[string]any{
				"path": path, "lines": float64(50),
				"grep": fmt.Sprintf("%s 3 ", tok),
			}, h.cfg, h.name)
		})
		gtxt := resText(rg)
		if !strings.Contains(gtxt, fmt.Sprintf("%s 3 ", tok)) {
			t.Errorf("grep result missing line 3: %s", gtxt)
		}
		if strings.Contains(gtxt, fmt.Sprintf("%s 1 ", tok)) {
			t.Errorf("grep result should NOT contain line 1: %s", gtxt)
		}
	})

	// --- streaming follow: prove the protocol loses nothing. ---
	t.Run("TailFollowNoLoss", func(t *testing.T) {
		h := newLiveHarness(t)
		tok := uniq("FOLLOW")
		path := h.tmp("follow")
		// Warm up 3s (>> dial latency) so follow is provably active
		// before line 1; then 5 lines 1s apart, all inside the
		// follow window. Generator self-caps at 40s.
		h.spawn("lines", tok, path, 3, 5, 1, 40)

		const follow = 15
		r := h.call("tail follow", (follow+12)*time.Second, func() toolResult {
			return handleTail(map[string]any{
				"path": path, "grep": tok,
				"follow_seconds": float64(follow), "lines": float64(1),
			}, h.cfg, h.name)
		})
		if r.IsError {
			t.Fatalf("tail follow errored: %s", resText(r))
		}
		txt := resText(r)
		var missing []string
		prev := -1
		for i := 1; i <= 5; i++ {
			marker := fmt.Sprintf("%s %d ", tok, i)
			idx := strings.Index(txt, marker)
			if idx < 0 {
				missing = append(missing, marker)
				continue
			}
			if idx <= prev {
				t.Errorf("%q out of order in stream (idx %d <= prev %d)", marker, idx, prev)
			}
			prev = idx
		}
		if len(missing) > 0 {
			t.Fatalf("STREAM DATA LOSS: %d/5 lines missing: %v\n--- got ---\n%s",
				len(missing), missing, txt)
		}
	})

	// --- journal: best-effort; skips cleanly on hosts without
	// journalctl/logger or where the journal isn't readable. ---
	t.Run("JournalOneShot", func(t *testing.T) {
		h := newLiveHarness(t)
		if out, _ := h.sh("command -v journalctl || true"); out == "" {
			t.Skip("remote has no journalctl")
		}
		if out, _ := h.sh("command -v logger || true"); out == "" {
			t.Skip("remote has no logger to inject a journal entry")
		}
		tag := uniq("JRNL")
		if _, code := h.sh(fmt.Sprintf("logger -t srvlive %s", shq(tag+" hello-from-livetest"))); code != 0 {
			t.Skip("logger failed (no syslog/journal sink)")
		}

		var txt string
		found := false
		h.waitFor("journal entry to surface", 12*time.Second, func() bool {
			r := handleJournal(map[string]any{
				"since": "5 min ago", "grep": tag, "lines": float64(200),
			}, h.cfg, h.name)
			txt = resText(r)
			if !r.IsError && strings.Contains(txt, tag) {
				found = true
				return true
			}
			return false
		})
		if !found {
			// Empty/with permission error => environment, not a code
			// defect. Don't fail the suite over host journal policy.
			if strings.Contains(txt, "Permission") || strings.TrimSpace(txt) == "" {
				t.Skipf("journal not readable for this user (env, not a code failure): %q", txt)
			}
			t.Fatalf("journal one-shot did not return injected tag %q:\n%s", tag, txt)
		}
	})

	// --- job log: spawn -> wait_job (completed) -> tail_log. ---
	t.Run("TailLogOfJob", func(t *testing.T) {
		h := newLiveHarness(t)
		tok := uniq("JOBLOG")
		rec := h.spawnRaw(fmt.Sprintf("for i in 1 2 3 4; do echo %s-$i; done", tok))

		rw := h.call("wait_job", 14*time.Second, func() toolResult {
			return handleWaitJob(map[string]any{
				"id": rec.ID, "max_wait_seconds": float64(10), "tail_lines": float64(20),
			}, h.cfg, h.name)
		})
		wt := resText(rw)
		if !strings.Contains(wt, "completed") || !strings.Contains(wt, "exit=0") {
			t.Fatalf("wait_job should report completed exit=0, got:\n%s", wt)
		}

		rl := h.call("tail_log", 12*time.Second, func() toolResult {
			return handleTailLog(map[string]any{
				"id": rec.ID, "lines": float64(50),
			}, h.cfg, h.name)
		})
		lt := resText(rl)
		for i := 1; i <= 4; i++ {
			if !strings.Contains(lt, fmt.Sprintf("%s-%d", tok, i)) {
				t.Errorf("tail_log missing %s-%d:\n%s", tok, i, lt)
			}
		}
	})

	// --- THE CENTREPIECE: killing one job must NOT kill another
	// ("sleep job 会不会误杀"). Proves setsid process-group isolation. ---
	t.Run("KillJobNoCollateral", func(t *testing.T) {
		h := newLiveHarness(t)
		jobA := h.spawn("idle", "A", "/dev/null", 0, 0, 0, 90)
		jobB := h.spawn("idle", "B", "/dev/null", 0, 0, 0, 90)

		h.waitFor("both idle jobs alive", 12*time.Second, func() bool {
			return h.alive(jobA) && h.alive(jobB)
		})

		// Sanity: detach must have placed them in DISTINCT process
		// groups. If not, a group-kill WOULD collaterally kill the
		// sibling -- the exact real-world risk under test.
		pgA, _ := h.sh(fmt.Sprintf("ps -o pgid= -p %d 2>/dev/null | tr -d ' '", jobA.Pid))
		pgB, _ := h.sh(fmt.Sprintf("ps -o pgid= -p %d 2>/dev/null | tr -d ' '", jobB.Pid))
		if pgA == "" || pgB == "" {
			t.Fatalf("could not read process groups (pgA=%q pgB=%q)", pgA, pgB)
		}
		if pgA == pgB {
			t.Fatalf("jobs share process group %s -- kill_job WOULD collaterally kill the sibling", pgA)
		}

		rk := h.call("kill_job A", 20*time.Second, func() toolResult {
			return handleKillJob(map[string]any{"id": jobA.ID}, h.cfg, h.name)
		})
		if rk.IsError {
			t.Fatalf("kill_job A errored: %s", resText(rk))
		}
		if kt := resText(rk); !strings.Contains(kt, "killed") {
			t.Fatalf("kill_job A should report killed, got: %s", kt)
		}

		// A's whole group must die...
		h.waitFor("job A fully dead", 15*time.Second, func() bool {
			out, _ := h.sh(fmt.Sprintf(
				"if kill -0 -%d 2>/dev/null || kill -0 %d 2>/dev/null; then echo UP; else echo DOWN; fi",
				jobA.Pid, jobA.Pid))
			return out == "DOWN"
		})
		// ...and B must be COMPLETELY UNAFFECTED (no 误杀).
		if !h.alive(jobB) {
			t.Fatalf("COLLATERAL KILL: job B died when only job A was killed")
		}
		// Hold B for a moment and re-check, to rule out a delayed
		// cascade through a shared ancestor.
		time.Sleep(2 * time.Second)
		if !h.alive(jobB) {
			t.Fatalf("COLLATERAL KILL (delayed): job B died after job A was killed")
		}

		// Now kill B too: proves the mechanism works on demand and
		// leaves nothing behind.
		_ = h.call("kill_job B", 20*time.Second, func() toolResult {
			return handleKillJob(map[string]any{"id": jobB.ID}, h.cfg, h.name)
		})
		h.waitFor("job B dead after explicit kill", 15*time.Second, func() bool {
			out, _ := h.sh(fmt.Sprintf("if kill -0 %d 2>/dev/null; then echo UP; else echo DOWN; fi", jobB.Pid))
			return out == "DOWN"
		})
	})

	// --- wait_job lifecycle: running -> completed(exit 0). ---
	t.Run("WaitJobRunningThenCompleted", func(t *testing.T) {
		h := newLiveHarness(t)
		rec := h.spawnRaw("sleep 6") // self-bounded

		r1 := h.call("wait_job (short)", 9*time.Second, func() toolResult {
			return handleWaitJob(map[string]any{
				"id": rec.ID, "max_wait_seconds": float64(2), "tail_lines": float64(5),
			}, h.cfg, h.name)
		})
		if t1 := resText(r1); !strings.Contains(t1, "running") {
			t.Fatalf("expected STATUS=running while sleep 6 in flight, got:\n%s", t1)
		}

		r2 := h.call("wait_job (long)", 18*time.Second, func() toolResult {
			return handleWaitJob(map[string]any{
				"id": rec.ID, "max_wait_seconds": float64(12), "tail_lines": float64(5),
			}, h.cfg, h.name)
		})
		if t2 := resText(r2); !strings.Contains(t2, "completed") || !strings.Contains(t2, "exit=0") {
			t.Fatalf("expected completed exit=0 after sleep finished, got:\n%s", t2)
		}
	})

	// --- prune_jobs: a finished job is discarded; a running one is
	// kept. Scoped to OUR job id so unrelated history is safe. ---
	t.Run("PruneKeepsRunning", func(t *testing.T) {
		h := newLiveHarness(t)
		done := h.spawnRaw("echo done") // finishes at once
		running := h.spawn("idle", "P", "/dev/null", 0, 0, 0, 60)

		_ = h.call("wait done", 12*time.Second, func() toolResult {
			return handleWaitJob(map[string]any{
				"id": done.ID, "max_wait_seconds": float64(8), "tail_lines": float64(3),
			}, h.cfg, h.name)
		})

		_ = h.call("prune done", 20*time.Second, func() toolResult {
			return handlePruneJobs(map[string]any{"id": done.ID}, h.cfg, h.name)
		})

		jf := jobs.Load()
		var sawDone, sawRunning bool
		for _, j := range jf.Jobs {
			if j.ID == done.ID {
				sawDone = true
			}
			if j.ID == running.ID {
				sawRunning = true
			}
		}
		if sawDone {
			t.Errorf("finished job %s should have been pruned", done.ID)
		}
		if !sawRunning {
			t.Errorf("running job %s must be kept by prune", running.ID)
		}
	})

	// --- >64 KiB ONE-SHOT: must reject cleanly with the oversize
	// receipt -- never truncate-and-return a sliced fragment, never
	// hang. Proves the cap is enforced and the body is withheld. ---
	t.Run("TailOneShotOversize", func(t *testing.T) {
		h := newLiveHarness(t)
		// Long token padding makes each generated line ~140 B so the
		// livegen 1000-line ceiling still clears the 64 KiB cap with
		// margin (~140 KiB). The pad is plain [A-Z0-9-] -> safe as a
		// literal grep pattern and shell-quoted arg.
		tok := uniq("OVR1") + strings.Repeat("X", 96)
		path := h.tmp("oversize1")
		// ~1000 lines * ~140 B ≈ ~140 KiB, far over the 64 KiB cap.
		// interval 0 -> one instant burst; generator self-caps at 30s.
		h.spawn("lines", tok, path, 0, 1000, 0, 30)
		h.waitFor("1000 generated lines", 25*time.Second, func() bool {
			out, _ := h.sh(fmt.Sprintf("grep -c %s %s 2>/dev/null || echo 0", shq(tok), path))
			return out == "1000"
		})

		r := h.call("tail oversize one-shot", 20*time.Second, func() toolResult {
			return handleTail(map[string]any{
				"path": path, "lines": float64(1000),
			}, h.cfg, h.name)
		})
		if !r.IsError {
			t.Fatalf("oversize one-shot must set IsError; got non-error:\n%s", resText(r))
		}
		sc := structMap(r)
		if sc == nil {
			t.Fatalf("oversize result missing structuredContent")
		}
		if sc["rejected_reason"] != "oversize_output" {
			t.Errorf("rejected_reason = %v, want oversize_output", sc["rejected_reason"])
		}
		if capB, _ := sc["cap_bytes"].(int); capB != ResultByteMax {
			t.Errorf("cap_bytes = %v, want %d", sc["cap_bytes"], ResultByteMax)
		}
		if got, _ := sc["bytes_returned"].(int); got <= ResultByteMax {
			t.Errorf("bytes_returned = %d, want > %d (file is ~140 KiB)", got, ResultByteMax)
		}
		// The body MUST NOT be returned: the contract is "model adds a
		// filter and retries", not "model reads a truncated slice that
		// may have dropped the relevant lines".
		if txt := resText(r); len(txt) > 1024 || strings.Contains(txt, tok) {
			t.Errorf("oversize body leaked (%d B, has token=%v); want a short receipt only",
				len(txt), strings.Contains(txt, tok))
		}
	})

	// --- HEAVY no-loss, CORRECTLY timed: this is the real test of
	// "does follow actually deliver the data". 350 distinct numbered
	// lines (~12 KiB total: deliberately > the ~8 KiB pipe block
	// buffer it crosses ~1.5x, but < the 64 KiB result cap so the
	// body is returned and every line is checkable). Lines are PACED
	// across the window AFTER a long warmup that covers BOTH dials
	// (spawnRaw's own + handleTail's) so `tail -F` is provably
	// attached before line 0 -- no pre-attach blast that `-n 1`
	// would collapse to a single backfilled line (the flaw in the
	// earlier version of this test). If plain RunStream passes this,
	// the block-buffering "data loss" does not reproduce and the
	// reverts were correct; if it fails, PTY/stdbuf is justified. ---
	t.Run("TailFollowHeavyNoLoss", func(t *testing.T) {
		h := newLiveHarness(t)
		tok := uniq("HEAVY")
		path := h.tmp("heavy")
		const lines = 350
		// sleep 6: > spawnRaw dial + handleTail dial, so tail -F is
		// attached and waiting on a not-yet-existing file before the
		// first write. 7 rounds * 50 lines with sleep 1 between ->
		// emission spans ~6s, every line a genuine in-window append.
		gen := fmt.Sprintf(
			`sleep 6; r=0; while [ $r -lt 7 ]; do n=0; while [ $n -lt 50 ]; do k=$((r*50+n)); printf '%s %%d\n' $k >> %s; n=$((n+1)); done; r=$((r+1)); sleep 1; done`,
			tok, path)
		h.spawnRaw(gen)
		h.tmpPath = append(h.tmpPath, path) // spawnRaw doesn't track paths; teardown rm

		const follow = 16 // > warmup(6) + emission(~6) + margin
		r := h.call("tail heavy follow", (follow+12)*time.Second, func() toolResult {
			return handleTail(map[string]any{
				"path": path, "grep": tok,
				"follow_seconds": float64(follow), "lines": float64(1),
			}, h.cfg, h.name)
		})
		if r.IsError {
			t.Fatalf("heavy follow errored: %s", resText(r))
		}
		txt := resText(r)

		// Independent ground truth: how many matching lines actually
		// landed on disk. The follow must deliver essentially all of
		// them; a few percent slack absorbs only the unavoidable
		// last-fraction-of-a-second deadline race, NOT block-buffer
		// loss (which destroys lines by the thousand).
		onDisk, _ := h.sh(fmt.Sprintf("grep -c %s %s 2>/dev/null || echo 0", shq(tok), path))

		var missing []string
		prev := -1
		for k := 0; k < lines; k++ {
			marker := fmt.Sprintf("%s %d\n", tok, k)
			idx := strings.Index(txt, marker)
			if idx < 0 {
				missing = append(missing, fmt.Sprintf("%d", k))
				continue
			}
			if idx <= prev {
				t.Errorf("line %d out of order (idx %d <= prev %d)", k, idx, prev)
			}
			prev = idx
		}
		if len(missing) > 0 {
			t.Fatalf("STREAM DATA LOSS: %d/%d lines missing from the follow "+
				"result (on disk: %s lines). If this is hundreds, the "+
				"block-buffered-follow bug is REAL and PTY/stdbuf is "+
				"justified; if ~0, it's the deadline tail-race only.\n"+
				"missing keys: %s",
				len(missing), lines, onDisk, strings.Join(missing, ","))
		}
	})

	// --- 误杀 check with a LITERAL `sleep`: killing one job must kill
	// its OWN sleep-child group and nothing else -- not a sibling
	// job's sleep, and absolutely not an unrelated srv-UNMANAGED
	// sleep the job system never spawned. ---
	t.Run("KillJobNoCollateralSleep", func(t *testing.T) {
		h := newLiveHarness(t)
		// Two independent jobs whose entire workload is a bare sleep
		// (self-bounded; teardown force-kills the group early anyway).
		jobA := h.spawnRaw("sleep 120")
		jobB := h.spawnRaw("sleep 120")
		// A third sleep srv knows NOTHING about: not in the ledger,
		// not in a tracked group. The strongest 误杀 disproof.
		unmanaged, _ := h.sh("nohup sleep 200 >/dev/null 2>&1 & echo $!")
		if unmanaged == "" {
			t.Fatalf("could not launch the unmanaged sleep")
		}
		// Always reap it, even on a failed assert mid-test.
		t.Cleanup(func() {
			_, _ = h.sh(fmt.Sprintf("kill -KILL %s 2>/dev/null; true", unmanaged))
		})
		alivePid := func(pid string) bool {
			out, _ := h.sh(fmt.Sprintf("if kill -0 %s 2>/dev/null; then echo UP; else echo DOWN; fi", pid))
			return out == "UP"
		}

		h.waitFor("both sleep jobs + unmanaged alive", 12*time.Second, func() bool {
			return h.alive(jobA) && h.alive(jobB) && alivePid(unmanaged)
		})
		pgA, _ := h.sh(fmt.Sprintf("ps -o pgid= -p %d 2>/dev/null | tr -d ' '", jobA.Pid))
		pgB, _ := h.sh(fmt.Sprintf("ps -o pgid= -p %d 2>/dev/null | tr -d ' '", jobB.Pid))
		if pgA == "" || pgB == "" || pgA == pgB {
			t.Fatalf("sleep jobs not in distinct process groups (pgA=%q pgB=%q)", pgA, pgB)
		}

		rk := h.call("kill_job A (sleep)", 20*time.Second, func() toolResult {
			return handleKillJob(map[string]any{"id": jobA.ID}, h.cfg, h.name)
		})
		if rk.IsError || !strings.Contains(resText(rk), "killed") {
			t.Fatalf("kill_job A should report killed, got: %s", resText(rk))
		}
		// A's whole group (its sleep child) must die.
		h.waitFor("job A sleep group fully dead", 15*time.Second, func() bool {
			out, _ := h.sh(fmt.Sprintf(
				"if kill -0 -%d 2>/dev/null || kill -0 %d 2>/dev/null; then echo UP; else echo DOWN; fi",
				jobA.Pid, jobA.Pid))
			return out == "DOWN"
		})
		// B's sleep AND the unmanaged sleep: completely untouched.
		if !h.alive(jobB) {
			t.Fatalf("误杀: sibling job B's sleep died when only A was killed")
		}
		if !alivePid(unmanaged) {
			t.Fatalf("误杀: unrelated UNMANAGED sleep (pid %s) died on kill_job A", unmanaged)
		}
		time.Sleep(2 * time.Second) // rule out a delayed cascade
		if !h.alive(jobB) || !alivePid(unmanaged) {
			t.Fatalf("误杀 (delayed): B.alive=%v unmanaged.alive=%v after kill A",
				h.alive(jobB), alivePid(unmanaged))
		}
		// Kill B on demand; the unmanaged sleep STILL must survive.
		_ = h.call("kill_job B (sleep)", 20*time.Second, func() toolResult {
			return handleKillJob(map[string]any{"id": jobB.ID}, h.cfg, h.name)
		})
		h.waitFor("job B sleep dead", 15*time.Second, func() bool {
			out, _ := h.sh(fmt.Sprintf("if kill -0 %d 2>/dev/null; then echo UP; else echo DOWN; fi", jobB.Pid))
			return out == "DOWN"
		})
		if !alivePid(unmanaged) {
			t.Fatalf("误杀: unmanaged sleep died after kill_job B too")
		}
	})
}
