package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"srv/internal/i18n"
	"srv/internal/picker"
	"srv/internal/session"
	"srv/internal/srvpath"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/theme"
	"strconv"
	"strings"
)

func cmdInit(cfg *Config) error {
	fmt.Println("Configure a new SSH profile (Ctrl+C to abort).")
	rd := bufio.NewReader(os.Stdin)
	name := prompt(rd, "profile name", "prod")
	if name == "" {
		return exitErr(1, "error: profile name required.")
	}
	host := prompt(rd, "host (ip or hostname)", "")
	if host == "" {
		return exitErr(1, "error: host required.")
	}
	defUser := os.Getenv("USER")
	if defUser == "" {
		defUser = os.Getenv("USERNAME")
	}
	user := prompt(rd, "user", defUser)
	portStr := prompt(rd, "port", "22")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return exitErr(1, "error: port must be a number.")
	}
	identity := prompt(rd, "identity file (blank = ssh default)", "")
	defaultCwd := prompt(rd, "default cwd", "~")

	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	cfg.Profiles[name] = &Profile{
		Host:         host,
		User:         user,
		Port:         port,
		IdentityFile: identity,
		DefaultCwd:   defaultCwd,
	}
	if cfg.DefaultProfile == "" {
		cfg.DefaultProfile = name
	}
	if err := SaveConfig(cfg); err != nil {
		return exitErr(1, "error: %v", err)
	}
	fmt.Printf("saved profile %q to %s\n", name, srvpath.Config())
	fmt.Println()
	fmt.Println("next: verify connectivity with `srv check` (it'll tell you exactly")
	fmt.Println("      what to fix if your key isn't in the server's authorized_keys).")
	return nil
}

func prompt(rd *bufio.Reader, q, def string) string {
	suffix := ""
	if def != "" {
		suffix = fmt.Sprintf(" [%s]", def)
	}
	fmt.Printf("%s%s: ", q, suffix)
	line, err := rd.ReadString('\n')
	if err != nil {
		fmt.Println()
		os.Exit(130)
	}
	v := strings.TrimSpace(line)
	if v == "" {
		return def
	}
	return v
}

func cmdConfig(args []string, cfg *Config) error {
	if len(args) == 0 {
		return exitErr(1, "%s", i18n.T("usage.config"))
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "global":
		return cmdConfigGlobal(rest, cfg)
	case "list":
		if len(cfg.Profiles) == 0 {
			fmt.Println(i18n.T("misc.no_profiles_run_init"))
			return nil
		}
		_, rec := session.Touch()
		var pinned string
		if rec.Profile != nil {
			pinned = *rec.Profile
		}
		// Stable order
		names := make([]string, 0, len(cfg.Profiles))
		for n := range cfg.Profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := cfg.Profiles[n]
			mark := "  "
			if n == pinned {
				mark = "@ "
			} else if n == cfg.DefaultProfile {
				mark = "* "
			}
			user := p.User
			target := fmt.Sprintf("%s@%s", user, p.Host)
			if user == "" {
				target = p.Host
			}
			fmt.Printf("%s%-16s %s:%d\n", mark, n, target, p.GetPort())
		}
		if pinned != "" {
			sid, _ := session.Touch()
			fmt.Printf("\n@ = pinned to this session (%s)\n", sid)
		}
		return nil
	case "default":
		// `srv config default` sets the global default profile (persists
		// to ~/.srv/config.json, applies across all shells). Distinct
		// from `srv use`, which only pins for the current shell session.
		if len(rest) == 0 {
			if srvtty.IsStdinTTY() {
				items := picker.BuildItems(cfg)
				sel, ok := picker.RunProfile(items, "Select global default profile (persists across shells):")
				if !ok {
					return nil
				}
				cfg.DefaultProfile = sel
				if err := SaveConfig(cfg); err != nil {
					return exitErr(1, "error: %v", err)
				}
				fmt.Printf("global default profile = %s\n", sel)
				return nil
			}
			// Non-TTY: print current.
			cur := cfg.DefaultProfile
			if cur == "" {
				cur = "(none)"
			}
			fmt.Printf("global default profile = %s\n", cur)
			return nil
		}
		if _, ok := cfg.Profiles[rest[0]]; !ok {
			return exitErr(1, "%s", i18n.T("err.profile_not_found", rest[0]))
		}
		cfg.DefaultProfile = rest[0]
		if err := SaveConfig(cfg); err != nil {
			return exitErr(1, "error: %v", err)
		}
		fmt.Printf("global default profile = %s\n", rest[0])
		return nil
	case "remove":
		if len(rest) == 0 {
			return exitErr(1, "%s", i18n.T("usage.config_rm"))
		}
		if _, ok := cfg.Profiles[rest[0]]; !ok {
			return exitErr(1, "%s", i18n.T("err.profile_not_found", rest[0]))
		}
		delete(cfg.Profiles, rest[0])
		if cfg.DefaultProfile == rest[0] {
			cfg.DefaultProfile = ""
			for k := range cfg.Profiles {
				cfg.DefaultProfile = k
				break
			}
		}
		if err := SaveConfig(cfg); err != nil {
			return exitErr(1, "error: %v", err)
		}
		fmt.Printf("removed %s\n", rest[0])
		return nil
	case "show":
		target := cfg.DefaultProfile
		if len(rest) > 0 {
			target = rest[0]
		}
		p, ok := cfg.Profiles[target]
		if !ok {
			return exitErr(1, "%s", i18n.T("err.profile_not_found", target))
		}
		out := map[string]*Profile{target: p}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	case "set":
		if len(rest) < 3 {
			return exitErr(1, "%s", i18n.T("usage.config_set"))
		}
		prof := rest[0]
		key := rest[1]
		value := strings.Join(rest[2:], " ")
		p, ok := cfg.Profiles[prof]
		if !ok {
			return exitErr(1, "%s", i18n.T("err.profile_not_found", prof))
		}
		applyProfileSet(p, key, value)
		if err := SaveConfig(cfg); err != nil {
			return exitErr(1, "error: %v", err)
		}
		fmt.Printf("%s.%s = %s\n", prof, key, value)
		return nil
	case "edit":
		target := cfg.DefaultProfile
		if len(rest) > 0 {
			target = rest[0]
		}
		if target == "" {
			return exitErr(1, "%s", i18n.T("usage.config_edit"))
		}
		p, ok := cfg.Profiles[target]
		if !ok {
			return exitErr(1, "%s", i18n.T("err.profile_not_found", target))
		}
		edited, err := editJSONValue(p, "srv-profile-*.json")
		if err != nil {
			return exitErr(1, "error: %v", err)
		}
		var next Profile
		if err := json.Unmarshal(edited, &next); err != nil {
			return exitErr(1, "error: edited profile is not valid JSON: %v", err)
		}
		cfg.Profiles[target] = &next
		if err := SaveConfig(cfg); err != nil {
			return exitErr(1, "error: %v", err)
		}
		fmt.Printf("updated profile %s\n", target)
		return nil
	}
	return exitErr(1, "%s", i18n.T("err.config_action", action))
}

// cmdConfigGlobal manages top-level (non-per-profile) config keys.
//
//	srv config global                    # list all globals + current values
//	srv config global <key>              # show one
//	srv config global <key> <value>      # set one
//	srv config global <key> --clear      # reset to default
func cmdConfigGlobal(args []string, cfg *Config) error {
	if len(args) == 0 {
		printGlobalConfig(cfg)
		return nil
	}
	key := args[0]
	if len(args) == 1 {
		return printOneGlobal(cfg, key)
	}
	value := args[1]
	clear := value == "--clear" || value == "-"

	switch key {
	case "hints":
		if clear {
			cfg.Hints = nil
		} else {
			b := strings.ToLower(value) == "true"
			cfg.Hints = &b
		}
	case "lang":
		if clear || strings.ToLower(value) == "auto" {
			cfg.Lang = ""
		} else {
			v := strings.ToLower(value)
			if v != "en" && v != "zh" {
				return exitErr(1, "%s", i18n.T("err.global_lang_value", value))
			}
			cfg.Lang = v
		}
	case "default_profile", "default":
		if clear {
			cfg.DefaultProfile = ""
		} else {
			if _, ok := cfg.Profiles[value]; !ok {
				return exitErr(1, "%s", i18n.T("err.profile_not_found", value))
			}
			cfg.DefaultProfile = value
		}
	default:
		return exitErr(1, "%s", i18n.T("err.global_unknown_key", key))
	}
	if err := SaveConfig(cfg); err != nil {
		return exitErr(1, "error: %v", err)
	}
	return printOneGlobal(cfg, key)
}

func printGlobalConfig(cfg *Config) {
	fmt.Printf("hints           = %s\n", boolDisplay(cfg.Hints, true))
	langDisplay := cfg.Lang
	if langDisplay == "" {
		langDisplay = "auto " + i18n.T("misc.global_lang_auto")
	}
	fmt.Printf("lang            = %s\n", langDisplay)
	def := cfg.DefaultProfile
	if def == "" {
		def = "(none)"
	}
	fmt.Printf("default_profile = %s\n", def)
}

func printOneGlobal(cfg *Config, key string) error {
	switch key {
	case "hints":
		fmt.Printf("hints = %s\n", boolDisplay(cfg.Hints, true))
	case "lang":
		v := cfg.Lang
		if v == "" {
			v = "auto"
		}
		fmt.Printf("lang = %s\n", v)
	case "default_profile", "default":
		v := cfg.DefaultProfile
		if v == "" {
			v = "(none)"
		}
		fmt.Printf("default_profile = %s\n", v)
	default:
		return exitErr(1, "%s", i18n.T("err.global_unknown_key", key))
	}
	return nil
}

// boolDisplay renders a *bool with explicit "(default true)" suffix
// when nil, so users can tell unset from explicitly-true.
func boolDisplay(p *bool, defaultVal bool) string {
	if p == nil {
		return fmt.Sprintf("%t (default)", defaultVal)
	}
	return fmt.Sprintf("%t", *p)
}

// applyProfileSet writes value into the profile field named by key. Bool
// strings ("true"/"false") and digit strings auto-convert.
func applyProfileSet(p *Profile, key, value string) {
	v := strings.ToLower(value)
	asBool := func() *bool {
		if v == "true" {
			return srvutil.BoolPtr(true)
		}
		return srvutil.BoolPtr(false)
	}
	switch key {
	case "host":
		p.Host = value
	case "user":
		p.User = value
	case "port":
		n, _ := strconv.Atoi(value)
		p.Port = n
	case "identity_file":
		if v == "" || v == "null" || v == "none" {
			p.IdentityFile = ""
		} else {
			p.IdentityFile = value
		}
	case "default_cwd":
		p.DefaultCwd = value
	case "multiplex":
		p.Multiplex = asBool()
	case "compression":
		p.Compression = asBool()
	case "compress_sync":
		p.CompressSync = asBool()
	case "connect_timeout":
		n, _ := strconv.Atoi(value)
		p.ConnectTimeout = n
	case "keepalive_interval":
		n, _ := strconv.Atoi(value)
		p.KeepaliveInterval = n
	case "keepalive_count":
		n, _ := strconv.Atoi(value)
		p.KeepaliveCount = n
	case "control_persist":
		p.ControlPersist = value
	case "dial_attempts":
		n, _ := strconv.Atoi(value)
		p.DialAttempts = n
	case "dial_backoff":
		p.DialBackoff = value
	case "sync_root":
		p.SyncRoot = value
	case "jump":
		// Comma-separated list of "[user@]host[:port]" hops. Empty / null
		// clears.
		if v == "" || v == "null" || v == "none" {
			p.Jump = nil
		} else {
			parts := strings.Split(value, ",")
			out := make([]string, 0, len(parts))
			for _, s := range parts {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
			p.Jump = out
		}
	case "env":
		if p.Env == nil {
			p.Env = map[string]string{}
		}
		for _, part := range strings.Split(value, ",") {
			k, val, ok := strings.Cut(strings.TrimSpace(part), "=")
			if ok && strings.TrimSpace(k) != "" {
				p.Env[strings.TrimSpace(k)] = val
			}
		}
	default:
		// unknown key -> store stringly
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra[key] = value
	}
}

func cmdUse(args []string, cfg *Config) error {
	if len(args) == 0 {
		// On a TTY, `srv use` opens an interactive picker that pins the
		// chosen profile to this shell session. Off a TTY (pipe / CI),
		// fall back to printing the current pin status so scripts that
		// already capture the output keep working.
		if srvtty.IsStdinTTY() && len(cfg.Profiles) > 0 {
			items := picker.BuildItems(cfg)
			sel, ok := picker.RunProfile(items, "Select a profile to pin to this shell:")
			if !ok {
				return nil
			}
			sid, err := SetSessionProfile(sel)
			if err != nil {
				return exitErr(1, "error: %v", err)
			}
			cwd := cfg.Profiles[sel].GetDefaultCwd()
			if c := GetCwd(sel, cfg.Profiles[sel]); c != "" {
				cwd = c
			}
			fmt.Printf("session %s: pinned to %q  (cwd: %s)\n", sid, sel, cwd)
			return nil
		}

		sid, rec := session.Touch()
		var pinned string
		if rec.Profile != nil {
			pinned = *rec.Profile
		}
		fmt.Printf("session : %s\n", sid)
		if pinned != "" {
			fmt.Printf("pinned  : %s\n", pinned)
		} else {
			fmt.Printf("pinned  : (none)\n")
		}
		if env := os.Getenv("SRV_PROFILE"); env != "" {
			fmt.Printf("env     : SRV_PROFILE=%s\n", env)
		}
		def := cfg.DefaultProfile
		if def == "" {
			def = "(none)"
		}
		fmt.Printf("default : %s\n", def)
		active := pinned
		if active == "" {
			active = os.Getenv("SRV_PROFILE")
		}
		if active == "" {
			active = cfg.DefaultProfile
		}
		if active == "" {
			active = "(none)"
		}
		fmt.Printf("active  : %s\n", active)
		return nil
	}
	a := args[0]
	if a == "--clear" || a == "-" || a == "-c" {
		sid, err := SetSessionProfile("")
		if err != nil {
			return exitErr(1, "error: %v", err)
		}
		fmt.Printf("session %s: unpinned\n", sid)
		return nil
	}
	if _, ok := cfg.Profiles[a]; !ok {
		return exitErr(1, "%s", i18n.T("err.profile_not_found", a))
	}
	sid, err := SetSessionProfile(a)
	if err != nil {
		return exitErr(1, "error: %v", err)
	}
	cwd := cfg.Profiles[a].GetDefaultCwd()
	if c := GetCwd(a, cfg.Profiles[a]); c != "" {
		cwd = c
	}
	fmt.Printf("session %s: pinned to %q  (cwd: %s)\n", sid, a, cwd)
	return nil
}

func cmdCd(path string, cfg *Config, profileOverride string) error {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	newCwd, err := changeRemoteCwd(name, profile, path)
	if err != nil {
		printDiagError(err, profile)
		return exitCode(1)
	}
	fmt.Println(newCwd)
	return nil
}

func cmdPwd(cfg *Config, profileOverride string) error {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	fmt.Println(GetCwd(name, profile))
	return nil
}

func cmdStatus(cfg *Config, profileOverride string) error {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	sid, rec := session.Touch()
	target := profile.Host
	if profile.User != "" {
		target = profile.User + "@" + profile.Host
	}
	pinned := ""
	if rec.Profile != nil {
		pinned = *rec.Profile
	}
	label := name + " (default)"
	if profileOverride != "" {
		label = name + " (-P override)"
	} else if pinned == name {
		label = name + " (pinned)"
	}
	fmt.Printf("profile : %s\n", label)
	fmt.Printf("target  : %s:%d\n", target, profile.GetPort())
	if profile.IdentityFile != "" {
		fmt.Printf("key     : %s\n", profile.IdentityFile)
	}
	fmt.Printf("cwd     : %s\n", GetCwd(name, profile))
	fmt.Printf("session : %s\n", sid)
	multiplex := profile.Multiplex == nil || *profile.Multiplex
	fmt.Printf("defaults: multiplex=%v  compression=%v  connect_timeout=%ds\n",
		multiplex, profile.GetCompression(), profile.GetConnectTimeout())
	return nil
}

func cmdShell(cfg *Config, profileOverride string) error {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	cwd := GetCwd(name, profile)
	c, err := Dial(profile)
	if err != nil {
		printDiagError(err, profile)
		return exitCode(255)
	}
	defer c.Close()
	rc, err := c.Shell(cwd)
	if err != nil {
		printDiagError(err, profile)
	}
	return exitCode(rc)
}

func cmdRun(args []string, cfg *Config, profileOverride string, tty bool) error {
	if len(args) == 0 {
		return exitErr(1, "error: nothing to run.")
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	cmd := strings.Join(args, " ")
	cmd = applyRemoteEnv(profile, cmd)
	// TTY mode allocates a real interactive shell on the remote that
	// sources ~/.bashrc itself, so colour just works -- skip our hook.
	// Non-TTY (`srv ls`, etc.) is what we have to fix up: see
	// theme.Prologue() for the rules. MCP never goes through this path.
	if !tty {
		if prologue := theme.Prologue(); prologue != "" {
			cmd = prologue + cmd
		}
	}
	cwd := GetCwd(name, profile)
	return exitCode(runRemoteStream(profile, cwd, cmd, tty))
}

func cmdPush(args []string, cfg *Config, profileOverride string) error {
	args, recursive := stripRecursive(args)
	if len(args) == 0 {
		return exitErr(1, "%s", i18n.T("usage.push"))
	}
	local := args[0]
	if _, err := os.Stat(local); err != nil {
		return exitErr(1, "%s", i18n.T("err.local_path_missing", local))
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	cwd := GetCwd(name, profile)
	remote := ""
	if len(args) > 1 {
		remote = args[1]
	} else {
		remote = baseName(local)
	}
	abs := resolveRemotePath(remote, cwd)
	rc, _, err := pushPath(profile, local, abs, recursive)
	if err != nil {
		printDiagError(err, profile)
	}
	return exitCode(rc)
}

func cmdPull(args []string, cfg *Config, profileOverride string) error {
	args, recursive := stripRecursive(args)
	if len(args) == 0 {
		return exitErr(1, "%s", i18n.T("usage.pull"))
	}
	remote := args[0]
	local := "."
	if len(args) > 1 {
		local = args[1]
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}
	cwd := GetCwd(name, profile)
	abs := resolveRemotePath(remote, cwd)
	rc, _, err := pullPath(profile, abs, local, recursive)
	if err != nil {
		printDiagError(err, profile)
	}
	return exitCode(rc)
}

func stripRecursive(args []string) ([]string, bool) {
	out := make([]string, 0, len(args))
	r := false
	for _, a := range args {
		if a == "-r" || a == "--recursive" {
			r = true
		} else {
			out = append(out, a)
		}
	}
	return out, r
}

func baseName(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func cmdSessions(args []string) error {
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	current := session.ID()
	all := session.All()
	switch action {
	case "list":
		if len(all) == 0 {
			fmt.Println("(no sessions)")
			return nil
		}
		ids := make([]string, 0, len(all))
		for id := range all {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rec := all[id]
			mark := "  "
			if id == current {
				mark = "> "
			}
			alive := "dead"
			if srvutil.PidAlive(id) {
				alive = "alive"
			}
			pinned := "-"
			if rec.Profile != nil {
				pinned = *rec.Profile
			}
			cwds := "-"
			if len(rec.Cwds) > 0 {
				parts := []string{}
				for k, v := range rec.Cwds {
					parts = append(parts, fmt.Sprintf("%s=%s", k, v))
				}
				sort.Strings(parts)
				cwds = strings.Join(parts, ", ")
			}
			fmt.Printf("%s%-8s %-6s profile=%-10s last_seen=%s  cwds=%s\n",
				mark, id, alive, pinned, rec.LastSeen, cwds)
		}
		fmt.Printf("\n> = current session (%s)\n", current)
		return nil
	case "show":
		_, _ = session.Touch()
		rec := session.All()[current]
		out := map[string]*session.Record{current: rec}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return nil
	case "clear":
		if session.Clear(current) {
			fmt.Printf("cleared session %s\n", current)
		} else {
			fmt.Printf("session %s has no record\n", current)
		}
		return nil
	case "prune":
		removed, before := session.PruneDead(srvutil.PidAlive)
		fmt.Printf("pruned %d/%d sessions (%d remaining)\n", removed, before, before-removed)
		return nil
	}
	return exitErr(1, "error: unknown sessions action %q", action)
}
