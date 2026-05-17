package check

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// RotateKey runs the full rotation flow:
//
//  1. Dial the server with the current profile creds (existing
//     identity file / agent / etc.). If this fails we abort -- we
//     can't push a new key without first proving access.
//  2. Generate a fresh ed25519 keypair locally under ~/.srv/keys.
//  3. Append the public key to remote ~/.ssh/authorized_keys
//     (creating the file/dir with proper perms when missing).
//  4. Re-dial using ONLY the new private key to verify it works.
//  5. Update profile.identity_file in config.json so subsequent
//     calls use the new key.
//  6. When `revokeOld` is true AND we can locate the prior pubkey
//     (profile.IdentityFile + ".pub" or `ssh-keygen -y` over the
//     prior private key), remove that exact line from
//     ~/.ssh/authorized_keys.
//
// The flow refuses to delete the old key entry without verifying the
// new key works -- otherwise a partial rotation could lock the user
// out of the only working credential.
func RotateKey(profile *config.Profile, profileName string, cfg *config.Config, revokeOld bool) error {
	fmt.Printf("rotate-key: %s\n", profileName)

	// 1. Sanity-dial.
	fmt.Println("  step 1/5: verifying current access...")
	c, err := sshx.Dial(profile)
	if err != nil {
		return srvutil.Errf(1, "cannot rotate -- current credentials don't connect: %v", err)
	}
	defer c.Close()

	// 2. Generate ed25519 keypair.
	fmt.Println("  step 2/5: generating ed25519 keypair...")
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return srvutil.Errf(1, "key gen: %v", err)
	}
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "host"
	}
	comment := fmt.Sprintf("srv-rotated@%s-%s", hostname, time.Now().Format("20060102"))
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return srvutil.Errf(1, "marshal private: %v", err)
	}
	privPEM := pem.EncodeToMemory(block)
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return srvutil.Errf(1, "wrap public: %v", err)
	}
	pubLine := ssh.MarshalAuthorizedKey(sshPub) // includes trailing newline
	pubWithComment := strings.TrimRight(string(pubLine), "\n") + " " + comment + "\n"

	// Write keys to ~/.srv/keys/<profile>-<timestamp>{,.pub}.
	keyDir := filepath.Join(srvutil.Dir(), "keys")
	if err := os.MkdirAll(keyDir, 0o700); err != nil {
		return srvutil.Errf(1, "mkdir keys: %v", err)
	}
	stamp := time.Now().Format("20060102-150405")
	keyPath := filepath.Join(keyDir, fmt.Sprintf("%s-%s", profileName, stamp))
	pubPath := keyPath + ".pub"
	if err := os.WriteFile(keyPath, privPEM, 0o600); err != nil {
		return srvutil.Errf(1, "write private key: %v", err)
	}
	if err := os.WriteFile(pubPath, []byte(pubWithComment), 0o644); err != nil {
		return srvutil.Errf(1, "write public key: %v", err)
	}
	fmt.Printf("    saved %s\n", keyPath)

	// 3. Append to authorized_keys (idempotent).
	fmt.Println("  step 3/5: pushing public key to ~/.ssh/authorized_keys...")
	quoted := srvtty.ShQuote(pubWithComment)
	authShell := `mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && grep -F -x ` +
		srvtty.ShQuote(strings.TrimRight(pubWithComment, "\n")) +
		` ~/.ssh/authorized_keys >/dev/null 2>&1 || printf '%s' ` + quoted + ` >> ~/.ssh/authorized_keys`
	res, err := c.RunCapture(authShell, "")
	if err != nil {
		return srvutil.Errf(1, "push pubkey: %v", err)
	}
	if res.ExitCode != 0 {
		return srvutil.Errf(1, "push pubkey: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	// 4. Verify by dialing with ONLY the new key.
	fmt.Println("  step 4/5: verifying new key authenticates...")
	probeProfile := *profile
	probeProfile.IdentityFile = keyPath
	// Clear any cached state and force a fresh agent-less dial: we
	// want to prove the new key works on its own.
	if err := dialOnlyWithKey(&probeProfile); err != nil {
		// Roll back: best-effort remove from authorized_keys so the
		// remote isn't littered with broken-key entries.
		fmt.Printf("    new key failed to authenticate (%v); rolling back authorized_keys entry...\n", err)
		_ = removeKeyLine(c, strings.TrimRight(pubWithComment, "\n"))
		_ = os.Remove(keyPath)
		_ = os.Remove(pubPath)
		return srvutil.Errf(1, "rotation aborted: %v", err)
	}

	// 5. Update profile.identity_file in config.
	fmt.Println("  step 5/5: updating profile.identity_file in config...")
	oldKey := profile.IdentityFile
	profile.IdentityFile = keyPath
	cfg.Profiles[profileName] = profile
	if err := config.Save(cfg); err != nil {
		return srvutil.Errf(1, "save config: %v", err)
	}

	fmt.Printf("\nrotated: %s now uses %s\n", profileName, keyPath)
	if tip := platformAwareTip(); tip != "" {
		fmt.Println(tip)
	}

	// 6. Optional revoke of old pubkey.
	if revokeOld {
		if oldKey == "" {
			fmt.Println("--revoke-old: profile had no identity_file; skipping (would need a pubkey to match against)")
			return nil
		}
		oldLine, lerr := readPubLine(oldKey)
		if lerr != nil {
			fmt.Printf("--revoke-old: could not derive old pubkey line (%v); skipping\n", lerr)
			return nil
		}
		if err := removeKeyLine(c, strings.TrimRight(oldLine, "\n")); err != nil {
			fmt.Printf("--revoke-old: remote scrub failed: %v\n", err)
			return nil
		}
		fmt.Println("--revoke-old: old pubkey line removed from authorized_keys")
	}
	return nil
}

// dialOnlyWithKey dials with a profile config that's been narrowed
// down to the freshly-rotated identity_file. Skipping the agent and
// other defaults is exactly the point -- we want to know the new key
// stands on its own.
func dialOnlyWithKey(p *config.Profile) error {
	// Temporarily unset SSH_AUTH_SOCK in the env so buildAuthMethods
	// doesn't fall through to the agent. Restored after the dial.
	saved := os.Getenv("SSH_AUTH_SOCK")
	_ = os.Unsetenv("SSH_AUTH_SOCK")
	defer func() {
		if saved != "" {
			_ = os.Setenv("SSH_AUTH_SOCK", saved)
		}
	}()
	c, err := sshx.Dial(p)
	if err != nil {
		return err
	}
	defer c.Close()
	res, err := c.RunCapture("echo srv-key-ok", "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "srv-key-ok") {
		return fmt.Errorf("post-rotation probe failed (exit %d, stderr %s)",
			res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// readPubLine derives the public key line for a private key file.
// First tries reading <privPath>.pub (the canonical openssh sidecar);
// falls back to parsing the private key and marshalling its public
// half, which works for any key srv can load.
func readPubLine(privPath string) (string, error) {
	pubPath := privPath + ".pub"
	if b, err := os.ReadFile(pubPath); err == nil {
		return strings.TrimRight(string(b), "\n") + "\n", nil
	}
	b, err := os.ReadFile(privPath)
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return "", err
	}
	return string(ssh.MarshalAuthorizedKey(signer.PublicKey())), nil
}

// removeKeyLine strips an exact pubkey line from the remote
// authorized_keys. Idempotent: missing line is not an error.
func removeKeyLine(c *sshx.Client, line string) error {
	q := srvtty.ShQuote(line)
	shell := `[ -f ~/.ssh/authorized_keys ] && grep -F -x -v ` + q + ` ~/.ssh/authorized_keys > ~/.ssh/authorized_keys.tmp && mv ~/.ssh/authorized_keys.tmp ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys`
	res, err := c.RunCapture(shell, "")
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// platformAwareTip surfaces a one-liner reminder for Windows users
// where .ssh perms are touchier than on POSIX. Returns "" for
// non-Windows hosts.
func platformAwareTip() string {
	if runtime.GOOS != "windows" {
		return ""
	}
	return "  note: on Windows the new key file's permissions follow NTFS ACL; ssh-add may complain " +
		"if it's group-readable. Use icacls to lock it down if you wire it into ssh-agent."
}
