// Package jobnotify holds the small set of side-effecting notification
// primitives (local OS toast, webhook POST) used by srv to announce
// detached-job completion. Lives in its own package so the daemon
// can call into it without picking up internal/jobcli's transitive
// dependency on internal/remote (which would create an import cycle:
// daemon -> jobcli -> remote -> daemon).
package jobnotify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"srv/internal/jobs"
	"srv/internal/platform"
	"time"
)

// Payload is the JSON body shape posted to webhooks and used for the
// local toast text. Stable across srv versions: webhook consumers
// should treat unknown fields as forward-compat additions.
type Payload struct {
	ID       string `json:"id"`
	Profile  string `json:"profile"`
	Cmd      string `json:"cmd"`
	Pid      int    `json:"pid"`
	Log      string `json:"log"`
	Started  string `json:"started"`
	Finished string `json:"finished"`
}

// ForJob is the canonical adapter from a *jobs.Record to a Payload.
func ForJob(j *jobs.Record) Payload {
	return Payload{
		ID:       j.ID,
		Profile:  j.Profile,
		Cmd:      j.Cmd,
		Pid:      j.Pid,
		Log:      j.Log,
		Started:  j.Started,
		Finished: j.Finished,
	}
}

// LocalToast pops a native OS notification by delegating to the
// platform package's Notifier. The OS-specific implementations
// (osascript on darwin, notify-send on linux, PowerShell Popup on
// windows) live in internal/platform/platform_<goos>_notify.go;
// this function just formats the title + body strings.
//
// Best-effort: a missing notifier tool returns an error the caller
// logs. The webhook side (see Webhook below) keeps working
// regardless, so a headless server with no libnotify still emits
// the post-job notification through that channel.
func LocalToast(p Payload) error {
	title := "srv: job " + p.ID + " finished"
	body := fmt.Sprintf("%s @ %s", truncate(p.Cmd, 80), p.Profile)
	return platform.Notif.Toast(title, body)
}

// Webhook POSTs the payload to url. 10s timeout; 2xx is success.
func Webhook(url string, p Payload) error {
	if url == "" {
		return fmt.Errorf("webhook url empty")
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "srv/job-notify")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %d", resp.StatusCode)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 4 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
