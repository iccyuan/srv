package install

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"srv/internal/platform"
	"strconv"
	"strings"
	"time"
)

//go:embed install.html
var installHTML []byte

// Snap is the small slice of package main state Cmd needs: the
// current binary version and a summary of the profile config. Caller
// in main builds it (LoadConfig + Version constant) and passes it
// in; this package never imports config or main directly.
type Snap struct {
	Version        string
	ProfileCount   int
	ProfileDefault string
}

// Cmd opens a browser-based installer for srv. It spins up a local
// HTTP server on a random localhost port, opens the default browser
// at it, and serves a single-page UI that talks to /api/* endpoints.
// The server exits when the user clicks Done or after a 10-minute
// idle.
//
// The same UI handles three platforms uniformly:
//   - "Add to PATH": Windows User env var, ~/.local/bin symlink on
//     Unix (or rc-file edit).
//   - "Register as Claude Code MCP" via `claude mcp add` user scope.
//   - "Register as Codex MCP" by editing ~/.codex/config.toml.
//   - "Run srv init" in a new terminal.
//
// Bootstrap entry points: ./install.ps1 -Gui and ./install.sh --gui
// both just locate srv and exec it with `install`. Power users can
// also run `srv install` directly once it's on PATH.
func Cmd(args []string, snap Snap) error {
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
		return fmt.Errorf("can't locate own binary: %v", err)
	}
	bin, _ = filepath.Abs(bin)

	// Per-run secret. The page we serve embeds it and sends it back in
	// X-Srv-Token on every /api/* call. A page from any other origin
	// can neither read our HTML (no CORS) nor send a custom header
	// without a preflight we never answer, so it cannot drive
	// /api/apply even though the port is guessable.
	token, err := newToken()
	if err != nil {
		return fmt.Errorf("token: %v", err)
	}
	page := bytes.ReplaceAll(installHTML, []byte(tokenPlaceholder), []byte(token))

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %v", err)
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
		_, _ = w.Write(page)
	})
	// api wraps every /api/* handler with the token + origin check.
	api := func(method string, fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !authorizeAPI(r, method, token, addr.String()) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			fn(w, r)
		}
	}
	mux.HandleFunc("/api/status", api(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		bumpIdle()
		s := buildInstallStatus(bin, snap)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("/api/apply", api(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		bumpIdle()
		var req struct {
			Actions []string `json:"actions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		log := applyInstallActions(bin, req.Actions)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"log": log})
	}))
	mux.HandleFunc("/api/quit", api(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
		go func() {
			time.Sleep(150 * time.Millisecond)
			select {
			case <-quit:
			default:
				close(quit)
			}
		}()
	}))

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

// tokenPlaceholder is substituted in install.html with the per-run token.
const tokenPlaceholder = "__SRV_TOKEN__"

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// authorizeAPI is the gate every /api/* request passes through. It
// requires the expected method, a matching X-Srv-Token, and -- when the
// browser sends them -- a Host/Origin that is our own listener. Compare
// in constant time; the token is the only thing standing between a
// hostile tab and "edit PATH / rewrite ~/.codex/config.toml".
func authorizeAPI(r *http.Request, method, token, listenAddr string) bool {
	if r.Method != method {
		return false
	}
	got := r.Header.Get("X-Srv-Token")
	if len(got) != len(token) || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		return false
	}
	if r.Host != "" && !sameListener(r.Host, listenAddr) {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" && !sameListener(strings.TrimPrefix(o, "http://"), listenAddr) {
		return false
	}
	return true
}

// sameListener reports whether host:port names our listener. The
// printed URL uses 127.0.0.1, but a user who retypes it as localhost
// must not be locked out, so both loopback spellings are accepted.
func sameListener(hostport, listenAddr string) bool {
	if hostport == listenAddr {
		return true
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return false
	}
	return hostport == "localhost:"+port
}

// installStatus is what /api/status returns -- the UI renders it directly.
type installStatus struct {
	Platform  string                 `json:"platform"`
	Binary    installStatusBinary    `json:"binary"`
	Path      installStatusPath      `json:"path"`
	ClaudeMcp installStatusClaudeMcp `json:"claude_mcp"`
	CodexMcp  installStatusCodexMcp  `json:"codex_mcp"`
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

type installStatusCodexMcp struct {
	ConfigPath string `json:"config_path"`
	Registered bool   `json:"registered"`
	Command    string `json:"command,omitempty"`
}

type installStatusProfiles struct {
	Count   int    `json:"count"`
	Default string `json:"default"`
}

func buildInstallStatus(bin string, snap Snap) installStatus {
	binDir := filepath.Dir(bin)
	available, registered, scope := detectClaudeMcp()
	codexRegistered, codexCommand, codexConfigPath := detectCodexMcp()
	return installStatus{
		Platform: runtime.GOOS,
		Binary:   installStatusBinary{Path: bin, Version: snap.Version},
		Path: installStatusPath{
			Dir:    binDir,
			OnPath: isOnPath(binDir),
		},
		ClaudeMcp: installStatusClaudeMcp{
			Available:  available,
			Registered: registered,
			Scope:      scope,
		},
		CodexMcp: installStatusCodexMcp{
			ConfigPath: codexConfigPath,
			Registered: codexRegistered,
			Command:    codexCommand,
		},
		Profiles: installStatusProfiles{Count: snap.ProfileCount, Default: snap.ProfileDefault},
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
		case "register_codex_mcp":
			if err := installRegisterCodexMcp(bin); err != nil {
				log = append(log, "Codex MCP: error: "+err.Error())
			} else {
				log = append(log, "Codex MCP: registered in "+codexConfigPath())
			}
		case "unregister_codex_mcp":
			if err := installUnregisterCodexMcp(); err != nil {
				log = append(log, "Codex MCP: error: "+err.Error())
			} else {
				log = append(log, "Codex MCP: removed from "+codexConfigPath())
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

func codexConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".codex", "config.toml")
	}
	return filepath.Join(home, ".codex", "config.toml")
}

func detectCodexMcp() (registered bool, command string, configPath string) {
	configPath = codexConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false, "", configPath
	}
	section := codexMcpSection(string(data))
	if section == "" {
		return false, "", configPath
	}
	return true, parseCodexMcpCommand(section), configPath
}

func installRegisterCodexMcp(bin string) error {
	path := codexConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	var text string
	if data, err := os.ReadFile(path); err == nil {
		text = string(data)
	} else if !os.IsNotExist(err) {
		return err
	}
	text, _ = removeCodexMcpSection(text)
	if strings.TrimSpace(text) != "" {
		text = strings.TrimRight(text, "\r\n") + "\n\n"
	}
	text += "[mcp_servers.srv]\n"
	text += "command = " + strconv.Quote(bin) + "\n"
	text += "args = [\"mcp\"]\n"
	return os.WriteFile(path, []byte(text), 0o600)
}

func installUnregisterCodexMcp() error {
	path := codexConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	text, removed := removeCodexMcpSection(string(data))
	if !removed {
		return nil
	}
	if strings.TrimSpace(text) != "" {
		text = strings.TrimRight(text, "\r\n") + "\n"
	}
	return os.WriteFile(path, []byte(text), 0o600)
}

func codexMcpSection(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[mcp_servers.srv]" {
			start = i
			continue
		}
		if start >= 0 && i > start && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			end = i
			break
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:end], "\n")
}

func removeCodexMcpSection(text string) (string, bool) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "[mcp_servers.srv]" {
			start = i
			continue
		}
		if start >= 0 && i > start && strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			end = i
			break
		}
	}
	if start < 0 {
		return text, false
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, lines[end:]...)
	return strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n", true
}

func parseCodexMcpCommand(section string) string {
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "command") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		raw := strings.TrimSpace(parts[1])
		if s, err := strconv.Unquote(raw); err == nil {
			return s
		}
		return strings.Trim(raw, `"'`)
	}
	return ""
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
	// The 3-way GOOS switch this used to be is exactly what
	// platform.Open.Open() does, so delegate. Keeps the
	// per-OS launcher logic in one place; if a new platform's
	// `xdg-open` equivalent gets added, this benefits for free.
	return platform.Open.Open(url)
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
