package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed install.html
var installHTML []byte

// cmdInstall opens a browser-based installer for srv. It spins up a local
// HTTP server on a random localhost port, opens the default browser at
// it, and serves a single-page UI that talks to /api/* endpoints. The
// server exits when the user clicks Done or after a 10-minute idle.
//
// The same UI handles three platforms uniformly:
//   - "Add to PATH": Windows User env var, ~/.local/bin symlink on Unix
//     (or rc-file edit), Codex/MCP registration if a CLI is detected.
//   - "Register as Claude Code MCP" via `claude mcp add` user scope.
//   - "Run srv init" in a new terminal.
//
// Bootstrap entry points: ./install.ps1 -Gui and ./install.sh --gui both
// just locate srv and exec it with `install`. Power users can also run
// `srv install` directly once it's on PATH.
func cmdInstall(args []string) error {
	noBrowser := false
	for _, a := range args {
		switch a {
		case "--no-browser":
			noBrowser = true
		case "--help", "-h":
			fmt.Println("usage: srv install [--no-browser]")
			return nil
		}
	}

	bin, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv install: can't locate own binary:", err)
		return exitCode(1)
	}
	bin, _ = filepath.Abs(bin)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv install: listen:", err)
		return exitCode(1)
	}
	addr := listener.Addr().(*net.TCPAddr)
	url := fmt.Sprintf("http://127.0.0.1:%d", addr.Port)

	quit := make(chan struct{})
	idleTimer := time.NewTimer(10 * time.Minute)
	bumpIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(10 * time.Minute)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		bumpIdle()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(installHTML)
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		bumpIdle()
		s := buildInstallStatus(bin)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("/api/apply", func(w http.ResponseWriter, r *http.Request) {
		bumpIdle()
		var req struct {
			Actions []string `json:"actions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		log := applyInstallActions(bin, req.Actions)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"log": log})
	})
	mux.HandleFunc("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
		go func() {
			time.Sleep(150 * time.Millisecond)
			select {
			case <-quit:
			default:
				close(quit)
			}
		}()
	})

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()

	fmt.Fprintf(os.Stderr, "srv installer listening at %s\n", url)
	if !noBrowser {
		if err := openBrowser(url); err != nil {
			fmt.Fprintf(os.Stderr, "(couldn't auto-open browser: %v -- copy the URL above)\n", err)
		}
	}

	select {
	case <-quit:
	case <-idleTimer.C:
		fmt.Fprintln(os.Stderr, "srv installer: idle timeout, shutting down.")
	}
	_ = server.Close()
	fmt.Fprintln(os.Stderr, "srv installer: done.")
	return nil
}

// installStatus is what /api/status returns -- the UI renders it directly.
type installStatus struct {
	Platform  string                 `json:"platform"`
	Binary    installStatusBinary    `json:"binary"`
	Path      installStatusPath      `json:"path"`
	ClaudeMcp installStatusClaudeMcp `json:"claude_mcp"`
	Profiles  installStatusProfiles  `json:"profiles"`
}

type installStatusBinary struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type installStatusPath struct {
	Dir    string `json:"dir"`
	OnPath bool   `json:"on_path"`
}

type installStatusClaudeMcp struct {
	Available  bool   `json:"available"`
	Registered bool   `json:"registered"`
	Scope      string `json:"scope"`
}

type installStatusProfiles struct {
	Count   int    `json:"count"`
	Default string `json:"default"`
}

func buildInstallStatus(bin string) installStatus {
	cfg, _ := LoadConfig()
	profCount := 0
	profDefault := ""
	if cfg != nil {
		profCount = len(cfg.Profiles)
		profDefault = cfg.DefaultProfile
	}
	binDir := filepath.Dir(bin)
	available, registered, scope := detectClaudeMcp()
	return installStatus{
		Platform: runtime.GOOS,
		Binary:   installStatusBinary{Path: bin, Version: Version},
		Path: installStatusPath{
			Dir:    binDir,
			OnPath: isOnPath(binDir),
		},
		ClaudeMcp: installStatusClaudeMcp{
			Available:  available,
			Registered: registered,
			Scope:      scope,
		},
		Profiles: installStatusProfiles{Count: profCount, Default: profDefault},
	}
}

func applyInstallActions(bin string, actions []string) []string {
	var log []string
	binDir := filepath.Dir(bin)
	for _, a := range actions {
		switch a {
		case "add_to_path":
			if err := installAddToPath(binDir); err != nil {
				log = append(log, "PATH: error: "+err.Error())
			} else {
				log = append(log, "PATH: added "+binDir+" (open a new terminal to pick it up)")
			}
		case "remove_from_path":
			if err := installRemoveFromPath(binDir); err != nil {
				log = append(log, "PATH: error: "+err.Error())
			} else {
				log = append(log, "PATH: removed "+binDir)
			}
		case "register_claude_mcp":
			if err := installRegisterClaudeMcp(bin); err != nil {
				log = append(log, "Claude MCP: error: "+err.Error())
			} else {
				log = append(log, "Claude MCP: registered (user scope)")
			}
		case "unregister_claude_mcp":
			if err := installUnregisterClaudeMcp(); err != nil {
				log = append(log, "Claude MCP: error: "+err.Error())
			} else {
				log = append(log, "Claude MCP: removed")
			}
		case "init_profile":
			if err := installSpawnInit(bin); err != nil {
				log = append(log, "init: error: "+err.Error())
			} else {
				log = append(log, "init: spawned in a new terminal window")
			}
		default:
			log = append(log, "ignored unknown action: "+a)
		}
	}
	return log
}

// isOnPath reports whether dir is a member of $PATH (string compare with
// trailing-separator normalization).
func isOnPath(dir string) bool {
	sep := string(os.PathListSeparator)
	norm := strings.TrimRight(dir, "/\\")
	for _, e := range strings.Split(os.Getenv("PATH"), sep) {
		if strings.TrimRight(e, "/\\") == norm {
			return true
		}
	}
	return false
}

// detectClaudeMcp checks whether the `claude` CLI exists and whether it
// already has an `srv` MCP server registered. Returns (available,
// registered, scope-string).
func detectClaudeMcp() (available, registered bool, scope string) {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return false, false, ""
	}
	available = true
	out, err := exec.Command(claudePath, "mcp", "get", "srv").CombinedOutput()
	if err != nil {
		return true, false, ""
	}
	text := string(out)
	// Heuristic parse: the `claude mcp get` output starts with `srv:`
	// followed by a `Scope:` line. A non-existent server returns an
	// error, which we already handled above.
	if !strings.HasPrefix(strings.TrimSpace(text), "srv:") {
		return true, false, ""
	}
	registered = true
	switch {
	case strings.Contains(text, "Scope: User"):
		scope = "user"
	case strings.Contains(text, "Scope: Project"):
		scope = "project"
	case strings.Contains(text, "Scope: Local"):
		scope = "local"
	}
	return
}

func installRegisterClaudeMcp(bin string) error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found on PATH (install Claude Code first)")
	}
	// Idempotency: drop any existing user-scope srv entry first so a
	// stale path (e.g. moved binary) gets corrected. Ignore the error;
	// the entry may not exist.
	_ = exec.Command(claudePath, "mcp", "remove", "srv", "-s", "user").Run()
	cmd := exec.Command(claudePath, "mcp", "add", "srv", "--scope", "user", "--", bin, "mcp")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude mcp add: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func installUnregisterClaudeMcp() error {
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		return fmt.Errorf("claude CLI not found on PATH")
	}
	cmd := exec.Command(claudePath, "mcp", "remove", "srv", "-s", "user")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("claude mcp remove: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// installSpawnInit opens a new terminal window running `srv init` so the
// user can complete the interactive prompt without losing the installer
// session. Best-effort -- on Linux without a known terminal emulator,
// returns an error telling the user to run it themselves.
func installSpawnInit(bin string) error {
	switch runtime.GOOS {
	case "windows":
		// `start` opens a new console; PowerShell -NoExit keeps it open
		// after `srv init` finishes so the user can read confirmation.
		return exec.Command("cmd", "/c", "start", "powershell", "-NoExit", "-Command",
			fmt.Sprintf("& '%s' init", strings.ReplaceAll(bin, "'", "''"))).Start()
	case "darwin":
		script := fmt.Sprintf(`tell application "Terminal" to do script "'%s' init"`,
			strings.ReplaceAll(bin, `"`, `\"`))
		return exec.Command("osascript", "-e", script).Start()
	default:
		for _, term := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xterm"} {
			if path, err := exec.LookPath(term); err == nil {
				return exec.Command(path, "-e", bin, "init").Start()
			}
		}
		return fmt.Errorf("no terminal emulator detected; run '%s init' manually", bin)
	}
}

// openBrowser opens the installer URL in the most appropriate window.
//
// Strategy:
//  1. Look for Microsoft Edge or Google Chrome (in that order; both ship
//     the `--app=URL` flag, which gives a chrome-less native-feeling
//     window with no tab bar / address bar). This is the preferred UX.
//  2. If neither is found, fall back to the OS default browser. The
//     installer still works, it just opens in a regular tab.
//
// Best-effort throughout: any failure at one step proceeds to the next.
func openBrowser(url string) error {
	if cmd := tryAppModeBrowser(url); cmd != nil {
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	return openDefaultBrowser(url)
}

func openDefaultBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

// tryAppModeBrowser returns an *exec.Cmd ready to launch a Chromium-
// based browser in `--app` mode (so the installer URL opens as a native-
// looking window, not a tab). Returns nil if no candidate is found.
func tryAppModeBrowser(url string) *exec.Cmd {
	args := []string{"--app=" + url, "--window-size=760,820"}
	for _, c := range chromiumCandidates() {
		// Try PATH-resolved name first (works when the browser registered
		// itself or is on $PATH).
		if path, err := exec.LookPath(c); err == nil {
			return exec.Command(path, args...)
		}
		// Try the absolute path (only if c looks like one).
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return exec.Command(c, args...)
			}
		}
	}
	return nil
}

func chromiumCandidates() []string {
	switch runtime.GOOS {
	case "windows":
		pf := os.Getenv("ProgramFiles")
		pfx86 := os.Getenv("ProgramFiles(x86)")
		local := os.Getenv("LOCALAPPDATA")
		out := []string{"msedge.exe", "chrome.exe"}
		for _, base := range []string{pfx86, pf, local} {
			if base == "" {
				continue
			}
			out = append(out,
				filepath.Join(base, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(base, "Google", "Chrome", "Application", "chrome.exe"),
			)
		}
		return out
	case "darwin":
		return []string{
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}
	default:
		return []string{
			"microsoft-edge", "microsoft-edge-stable",
			"google-chrome", "google-chrome-stable",
			"chromium", "chromium-browser",
		}
	}
}
