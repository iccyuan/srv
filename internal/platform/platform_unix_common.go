//go:build !windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// errUnsupportedNotifier is a unix-side sentinel for "this platform
// doesn't have a notifier hooked up." BSD fallbacks return it so
// callers can errors.Is() against a single value rather than
// string-matching error text. Linux + macOS return real exec errors
// when their notifier tool is missing; the sentinel is only for
// "not implemented" paths.
var errUnsupportedNotifier = errors.New("no local notifier available")

// This file holds the POSIX-portable pieces every unix-like target
// shares -- the bits that depend on Setsid / SIGTERM / signal 0 /
// SIGWINCH and nothing more specific. Linux's /proc parsing,
// macOS's sysctl, and BSD-specific behaviours live in their own
// per-OS files (platform_linux.go, platform_darwin.go,
// platform_bsd.go). Each per-OS file embeds the structs here, adds
// what it needs, and runs the init() that installs the right
// implementation into the package-level vars.
//
// "What it needs" today is just PIDStartTime: only Linux has /proc
// to read it from cheaply, so the base struct deliberately omits
// that method and forces each OS file to provide its own.

// --- Process base -------------------------------------------------

// unixProcessBase carries the methods that are identical across all
// unix-likes. Per-OS files embed this and add PIDStartTime (the
// only Process method that has OS-specific behaviour today).
type unixProcessBase struct{}

func (unixProcessBase) Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func (unixProcessBase) SignalTerminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

func (unixProcessBase) PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		if err == syscall.ESRCH {
			return false
		}
	}
	return true
}

// --- Console (full impl, identical across unix-likes) ------------

type unixConsole struct{}

func (unixConsole) WatchWindowResize(onResize func(cols, rows int)) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	stop := make(chan struct{})
	go func() {
		defer signal.Stop(sigCh)
		for {
			select {
			case <-sigCh:
				if w, h := unixConsoleSize(); w > 0 && h > 0 {
					onResize(w, h)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}

func (unixConsole) EnableLocalVTOutput() func() {
	return func() {}
}

// unixConsoleSize wraps golang.org/x/term.GetSize. Kept here rather
// than reaching into srvtty.Size to keep the platform package at
// the bottom of the dependency graph.
func unixConsoleSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

// --- Crypto (no-op, identical across unix-likes) -----------------

type unixCrypto struct{}

func (unixCrypto) HardenKeyFile(_ string) error {
	return nil
}

// --- Opener: default xdg-open ------------------------------------

// xdgOpener works on every freedesktop-compliant unix (most Linux
// distros, FreeBSD with the right ports, OpenBSD with xdg-utils
// installed). macOS overrides this with its own `open` command --
// see platform_darwin.go.
type xdgOpener struct{}

func (xdgOpener) Open(path string) error {
	return exec.Command("xdg-open", path).Start()
}

// --- Shell (identical across unix-likes) -------------------------

// unixShell honours the user's $SHELL preference (the same lookup
// pattern OpenSSH uses for `ssh -t host '<cmd>'`), falling back to
// /bin/sh when the env var is unset (cron jobs, minimal containers).
// Posix shells all support -c so the same invocation works for
// bash / zsh / dash / ash without per-shell branching.
type unixShell struct{}

func (unixShell) Command(cmd string) *exec.Cmd {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return exec.Command(shell, "-c", cmd)
}
