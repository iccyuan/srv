//go:build linux

package platform

import "os/exec"

// linuxNotifier shells out to notify-send (libnotify) which is the
// de-facto standard on every desktop Linux. Headless servers
// usually don't have it; the caller's job-completion watcher
// already logs the error and continues.
//
// Distro coverage: notify-send ships in:
//   - Ubuntu / Debian: libnotify-bin
//   - Fedora / RHEL: libnotify (often default)
//   - Arch: libnotify
//   - Alpine: libnotify (manual install)
//
// We don't try any fallback notifiers (zenity --notification,
// kdialog --passivepopup, dunstify) because the per-DE matrix gets
// large fast for marginal benefit; users on those DEs typically
// have notify-send too.
type linuxNotifier struct{}

func (linuxNotifier) Toast(title, body string) error {
	cmd := exec.Command("notify-send", "-a", "srv", title, body)
	return cmd.Run()
}
