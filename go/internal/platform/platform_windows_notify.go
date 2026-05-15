//go:build windows

package platform

import (
	"fmt"
	"os/exec"
)

// windowsNotifier pops a balloon notification via PowerShell's
// WScript.Shell.Popup. No third-party module needed, no toast
// permission flow -- it just appears.
//
// We deliberately don't use the modern toast notification API
// (Windows.UI.Notifications.ToastNotification) because that
// requires the calling app to be COM-registered with a package
// identity, which is overkill for a CLI's "your job finished"
// blip. The popup gets shut down by the user clicking OK or by
// the 5-second timeout we pass (the second arg to Popup).
type windowsNotifier struct{}

func (windowsNotifier) Toast(title, body string) error {
	ps := fmt.Sprintf(
		`$w=New-Object -ComObject WScript.Shell;$w.Popup(%q,5,%q,64) | Out-Null`,
		body, title,
	)
	return exec.Command("powershell", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps).Run()
}
