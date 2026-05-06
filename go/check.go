package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// CheckResult is what _ssh_check returns in the Python version.
type CheckResult struct {
	OK        bool   `json:"ok"`
	Diagnosis string `json:"diagnosis"`
	ExitCode  int    `json:"exit_code"`
	Stderr    string `json:"stderr"`
}

// runCheck probes the server with a strict, no-prompt config and a 15s
// timeout. Returns a diagnosis tag matching the Python version's set:
// ok / no-key / host-key-changed / dns / refused / no-route /
// tcp-timeout / timeout / perm-denied / unknown.
func runCheck(profile *Profile) *CheckResult {
	res := &CheckResult{}

	done := make(chan *CheckResult, 1)
	go func() {
		c, err := DialOpts(profile, dialOpts{
			strictHostKey: false, // accept-new like the Python version
			timeout:       time.Duration(profile.GetConnectTimeout()) * time.Second,
		})
		if err != nil {
			done <- &CheckResult{
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
			done <- &CheckResult{OK: true, Diagnosis: "ok"}
			return
		}
		stderr := ""
		exit := -1
		if r != nil {
			stderr = r.Stderr
			exit = r.ExitCode
		}
		done <- &CheckResult{
			OK:        false,
			Diagnosis: "unknown",
			ExitCode:  exit,
			Stderr:    stderr,
		}
	}()

	select {
	case res = <-done:
	case <-time.After(15 * time.Second):
		res = &CheckResult{
			OK:        false,
			Diagnosis: "timeout",
			ExitCode:  -1,
		}
	}
	return res
}

// classifyDialError maps a Go SSH dial error into a stable diagnosis tag.
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

// checkAdvice returns the actionable lines for a given diagnosis.
func checkAdvice(diag string, profile *Profile, profileName string) []string {
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

func cmdCheck(cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
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

	res := runCheck(profile)

	if res.OK {
		fmt.Println("OK -- connected; key authentication works.")
		return 0
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
	for _, line := range checkAdvice(res.Diagnosis, profile, name) {
		fmt.Println(line)
	}
	return 1
}
