// Package check probes SSH connectivity for a profile: dials with a
// strict no-prompt config and a 15 s watchdog, classifies failures
// into stable diagnosis tags (no-key / host-key-changed / dns /
// refused / no-route / tcp-timeout / timeout / perm-denied /
// unknown), and renders actionable fix advice.
//
// The same Run() result feeds both the `srv check` CLI subcommand
// (Cmd) and the MCP `check` tool handler. PrintDialError is the
// "best-effort diagnosis if this looks like an SSH failure" wrapper
// used by other CLI commands when their own SSH dial fails.
package check

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/sshx"
	"strconv"
	"strings"
	"time"
)

// Result mirrors the Python `_ssh_check` payload: a bool plus a
// stable diagnosis tag, plus whatever stderr / exit code the probe
// captured before deciding.
type Result struct {
	OK        bool   `json:"ok"`
	Diagnosis string `json:"diagnosis"`
	ExitCode  int    `json:"exit_code"`
	Stderr    string `json:"stderr"`
}

// Run probes the server with a strict, no-prompt config and a 15 s
// timeout. Returns a diagnosis tag matching the Python version's
// set: ok / no-key / host-key-changed / dns / refused / no-route /
// tcp-timeout / timeout / perm-denied / unknown.
func Run(profile *config.Profile) *Result {
	res := &Result{}

	// The dial budget must stay strictly inside the outer 15 s
	// timeout -- otherwise a profile with a large `connect_timeout`
	// (some users set 30 s or 60 s for high-latency links) leaves
	// the inner goroutine blocked in Dial well after Run has
	// already returned a "timeout" verdict, briefly piling up under
	// repeated checks.
	dialTimeout := time.Duration(profile.GetConnectTimeout()) * time.Second
	if dialTimeout <= 0 || dialTimeout > 14*time.Second {
		dialTimeout = 14 * time.Second
	}

	done := make(chan *Result, 1)
	go func() {
		c, err := sshx.DialOpts(profile, sshx.DialOptions{
			StrictHostKey: false, // accept-new like the Python version
			Timeout:       dialTimeout,
		})
		if err != nil {
			done <- &Result{
				OK:        false,
				Diagnosis: classifyDialError(err),
				ExitCode:  255,
				Stderr:    err.Error(),
			}
			return
		}
		defer c.Close()
		r, _ := c.RunCapture("echo srv-check-ok", "")
		if r != nil && r.ExitCode == 0 && strings.Contains(r.Stdout, "srv-check-ok") {
			done <- &Result{OK: true, Diagnosis: "ok"}
			return
		}
		stderr := ""
		exit := -1
		if r != nil {
			stderr = r.Stderr
			exit = r.ExitCode
		}
		done <- &Result{
			OK:        false,
			Diagnosis: "unknown",
			ExitCode:  exit,
			Stderr:    stderr,
		}
	}()

	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		res = &Result{
			OK:        false,
			Diagnosis: "timeout",
			ExitCode:  -1,
		}
	}
	return res
}

// classifyDialError maps a Go SSH dial error into a stable diagnosis
// tag. Internal -- callers that need the tag go through Run() (which
// embeds it in Result.Diagnosis) or PrintDialError.
func classifyDialError(err error) string {
	msg := strings.ToLower(err.Error())

	// crypto/ssh errors for auth/host-key
	if strings.Contains(msg, "ssh: handshake failed") {
		if strings.Contains(msg, "unable to authenticate") || strings.Contains(msg, "no supported methods remain") {
			return "no-key"
		}
		if strings.Contains(msg, "host key") || strings.Contains(msg, "knownhosts") {
			return "host-key-changed"
		}
	}
	if strings.Contains(msg, "knownhosts: key mismatch") || strings.Contains(msg, "host key changed") {
		return "host-key-changed"
	}

	// Network-level errors -- inspect underlying types when possible.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return "tcp-timeout"
		}
		s := strings.ToLower(opErr.Error())
		if strings.Contains(s, "refused") {
			return "refused"
		}
		if strings.Contains(s, "no route to host") || strings.Contains(s, "network is unreachable") {
			return "no-route"
		}
	}

	// Fall through string matches.
	switch {
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "timed out"):
		return "tcp-timeout"
	case strings.Contains(msg, "no such host"):
		return "dns"
	case strings.Contains(msg, "connection refused"):
		return "refused"
	case strings.Contains(msg, "no route to host"):
		return "no-route"
	case strings.Contains(msg, "permission denied"):
		return "no-key"
	}
	return "unknown"
}

// Advice returns the actionable lines for a given diagnosis. Used
// by both the CLI (Cmd) and the MCP check tool to render the
// fix-it section after a failure.
func Advice(diag string, profile *config.Profile, profileName string) []string {
	user := profile.User
	host := profile.Host
	port := profile.GetPort()
	target := host
	if user != "" {
		target = user + "@" + host
	}
	identity := profile.IdentityFile
	if identity == "" {
		identity = "~/.ssh/id_rsa"
	}
	pub := identity
	if !strings.HasSuffix(pub, ".pub") {
		pub = identity + ".pub"
	}

	switch diag {
	case "no-key":
		return []string{
			"key authentication rejected -- your local public key is NOT in the",
			"server's authorized_keys.",
			"",
			"Fix it (pick one):",
			fmt.Sprintf("  ssh-copy-id -i %s %s", pub, target),
			"  # PowerShell equivalent (no ssh-copy-id on Windows):",
			fmt.Sprintf("  type %s | ssh %s \"cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys\"",
				strings.ReplaceAll(pub, "~", "$env:USERPROFILE"), target),
			"",
			"After authorizing, re-run: srv check",
		}
	case "host-key-changed":
		return []string{
			"server host key doesn't match the one in known_hosts (or first connection",
			"with strict host-key checking).",
			"",
			"If you trust this is the same server and the key really changed:",
			fmt.Sprintf("  ssh-keygen -R %s", host),
			fmt.Sprintf("  ssh-keyscan -H %s >> ~/.ssh/known_hosts", host),
			"",
			"Otherwise verify with the server's admin first.",
		}
	case "dns":
		return []string{
			fmt.Sprintf("can't resolve hostname %q.", host),
			fmt.Sprintf("  - check the host spelling: srv config show %s", profileName),
		}
	case "refused":
		return []string{
			fmt.Sprintf("connection refused at %s:%d.", host, port),
			"  - is sshd running on the server?",
			fmt.Sprintf("  - is port %d correct? (try: srv config set %s port <N>)", port, profileName),
			"  - is a firewall blocking?",
		}
	case "no-route":
		return []string{
			fmt.Sprintf("no route to %s.", host),
			"  network unreachable -- check VPN / firewall / interface state.",
		}
	case "tcp-timeout":
		return []string{
			fmt.Sprintf("connection timed out reaching %s:%d.", host, port),
			"  - server may be down or unreachable",
			"  - firewall may be silently dropping",
			fmt.Sprintf("  - try a longer timeout: srv config set %s connect_timeout 30", profileName),
		}
	case "timeout":
		return []string{
			"probe took longer than the watchdog (15s). Server may be hanging on",
			fmt.Sprintf("connection setup; try interactively: ssh %s", target),
		}
	case "perm-denied":
		return []string{
			"auth failed -- the server didn't accept any of your keys, and",
			"password auth is either disabled or batch mode prevented prompting.",
			"Verify which key the server expects, and ensure it's in authorized_keys.",
		}
	}
	return []string{"unknown failure mode -- see stderr above."}
}

// PrintDialError writes an error to stderr, augmenting with a
// diagnosis tag and actionable fix steps if the error matches a
// known SSH failure mode (no-key / refused / dns / etc.). Falls back
// to the raw error otherwise. Called by other CLI commands when
// their own dial fails -- centralises the "explain why this looks
// broken" surface so every command's failure path produces the same
// advice.
func PrintDialError(err error, profile *config.Profile) {
	if err == nil {
		return
	}
	diag := classifyDialError(err)
	if diag == "" || diag == "unknown" {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Fprintf(os.Stderr, "srv: %v\n", err)
	fmt.Fprintf(os.Stderr, "diagnosis: %s\n\n", diag)
	name := ""
	if profile != nil {
		name = profile.Name
	}
	for _, line := range Advice(diag, profile, name) {
		fmt.Fprintln(os.Stderr, line)
	}
}

// Cmd implements `srv check [--rtt [--count N] [--interval D]]`.
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	rtt := false
	count := 10
	interval := 200 * time.Millisecond
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--rtt":
			rtt = true
		case a == "--count" && i+1 < len(args):
			if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
				count = n
			}
			i++
		case strings.HasPrefix(a, "--count="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--count=")); err == nil && n > 0 {
				count = n
			}
		case a == "--interval" && i+1 < len(args):
			if d, err := time.ParseDuration(args[i+1]); err == nil && d > 0 {
				interval = d
			}
			i++
		case strings.HasPrefix(a, "--interval="):
			if d, err := time.ParseDuration(strings.TrimPrefix(a, "--interval=")); err == nil && d > 0 {
				interval = d
			}
		default:
			if strings.HasPrefix(a, "-") {
				return clierr.Errf(1, "error: unknown check flag %q", a)
			}
		}
	}

	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}

	if rtt {
		return clierr.Code(runRTTProbe(profile, name, count, interval))
	}

	user := profile.User
	host := profile.Host
	target := host
	if user != "" {
		target = user + "@" + host
	}
	fmt.Printf("checking %s: %s:%d\n", name, target, profile.GetPort())
	if profile.IdentityFile != "" {
		fmt.Printf("  key : %s\n", profile.IdentityFile)
	} else {
		fmt.Printf("  key : (ssh default search; commonly ~/.ssh/id_rsa or id_ed25519)\n")
	}
	fmt.Println()

	res := Run(profile)

	if res.OK {
		fmt.Println("OK -- connected; key authentication works.")
		return nil
	}
	fmt.Printf("FAIL (%s; exit %d)\n", res.Diagnosis, res.ExitCode)
	if res.Stderr != "" {
		fmt.Println()
		fmt.Println("error:")
		for _, line := range strings.Split(strings.TrimRight(res.Stderr, "\n"), "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println()
	for _, line := range Advice(res.Diagnosis, profile, name) {
		fmt.Println(line)
	}
	return clierr.Code(1)
}

// runRTTProbe times `count` SSH-level keepalive round trips against
// the profile's server and prints a per-probe + summary report.
// Useful when you want to know whether `srv` is slow because of the
// server, the link, or your local environment.
//
// Returns 1 if more than half the probes were lost (link is
// unusable), otherwise 0.
func runRTTProbe(profile *config.Profile, name string, count int, interval time.Duration) int {
	user := profile.User
	host := profile.Host
	target := host
	if user != "" {
		target = user + "@" + host
	}
	fmt.Printf("rtt probe %s: %s:%d  (%d samples, %v interval)\n\n", name, target, profile.GetPort(), count, interval)

	c, err := sshx.DialOpts(profile, sshx.DialOptions{StrictHostKey: false})
	if err != nil {
		PrintDialError(err, profile)
		return 1
	}
	defer c.Close()

	samples := make([]time.Duration, 0, count)
	lost := 0
	for i := 0; i < count; i++ {
		start := time.Now()
		_, _, err := c.Conn.SendRequest("keepalive@openssh.com", true, nil)
		elapsed := time.Since(start)
		if err != nil {
			lost++
			fmt.Printf("%3d/%d   lost   (%v)\n", i+1, count, err)
		} else {
			samples = append(samples, elapsed)
			fmt.Printf("%3d/%d   %v\n", i+1, count, elapsed.Truncate(time.Microsecond))
		}
		if i < count-1 {
			time.Sleep(interval)
		}
	}

	fmt.Println()
	if len(samples) == 0 {
		fmt.Println("all probes lost -- connection up but no replies. Check server keepalive policy.")
		return 1
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	min := samples[0]
	max := samples[len(samples)-1]
	var sum time.Duration
	for _, s := range samples {
		sum += s
	}
	avg := sum / time.Duration(len(samples))
	median := samples[len(samples)/2]
	loss := float64(lost) * 100 / float64(count)

	tr := func(d time.Duration) time.Duration { return d.Truncate(time.Microsecond) }
	fmt.Printf("samples : %d ok, %d lost  (loss %.1f%%)\n", len(samples), lost, loss)
	fmt.Printf("rtt     : min %v  med %v  avg %v  max %v\n", tr(min), tr(median), tr(avg), tr(max))

	jitter := tr(max - min)
	switch {
	case loss >= 5:
		fmt.Printf("verdict : packet loss is high (%.1f%%); link is flaky\n", loss)
	case avg > 200*time.Millisecond:
		fmt.Printf("verdict : high latency (avg %v); commands will feel slow\n", tr(avg))
	case jitter > 100*time.Millisecond:
		fmt.Printf("verdict : noticeable jitter (max-min = %v)\n", jitter)
	default:
		fmt.Println("verdict : link looks healthy")
	}

	if lost*2 > count {
		return 1
	}
	return 0
}
