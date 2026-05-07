package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

func cmdInit(cfg *Config) int {
	fmt.Println("Configure a new SSH profile (Ctrl+C to abort).")
	rd := bufio.NewReader(os.Stdin)
	name := prompt(rd, "profile name", "prod")
	if name == "" {
		fatal("error: profile name required.")
	}
	host := prompt(rd, "host (ip or hostname)", "")
	if host == "" {
		fatal("error: host required.")
	}
	defUser := os.Getenv("USER")
	if defUser == "" {
		defUser = os.Getenv("USERNAME")
	}
	user := prompt(rd, "user", defUser)
	portStr := prompt(rd, "port", "22")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		fatal("error: port must be a number.")
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
		fatal("error: %v", err)
	}
	fmt.Printf("saved profile %q to %s\n", name, ConfigFile())
	fmt.Println()
	fmt.Println("next: verify connectivity with `srv check` (it'll tell you exactly")
	fmt.Println("      what to fix if your key isn't in the server's authorized_keys).")
	return 0
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

func cmdConfig(args []string, cfg *Config) int {
	if len(args) == 0 {
		fatal("usage: srv config <list|use|remove|show|set> [args]")
	}
	action := args[0]
	rest := args[1:]
	switch action {
	case "list":
		if len(cfg.Profiles) == 0 {
			fmt.Println("(no profiles configured -- run `srv init`)")
			return 0
		}
		_, rec := TouchSession()
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
			sid, _ := TouchSession()
			fmt.Printf("\n@ = pinned to this session (%s)\n", sid)
		}
		return 0
	case "use":
		if len(rest) == 0 {
			fatal("usage: srv config use <name>  (sets global default; for per-shell switching use `srv use <name>`)")
		}
		if _, ok := cfg.Profiles[rest[0]]; !ok {
			fatal("error: profile %q not found.", rest[0])
		}
		cfg.DefaultProfile = rest[0]
		if err := SaveConfig(cfg); err != nil {
			fatal("error: %v", err)
		}
		fmt.Printf("global default profile = %s\n", rest[0])
		return 0
	case "remove":
		if len(rest) == 0 {
			fatal("usage: srv config remove <name>")
		}
		if _, ok := cfg.Profiles[rest[0]]; !ok {
			fatal("error: profile %q not found.", rest[0])
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
			fatal("error: %v", err)
		}
		fmt.Printf("removed %s\n", rest[0])
		return 0
	case "show":
		target := cfg.DefaultProfile
		if len(rest) > 0 {
			target = rest[0]
		}
		p, ok := cfg.Profiles[target]
		if !ok {
			fatal("error: profile %q not found.", target)
		}
		out := map[string]*Profile{target: p}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	case "set":
		if len(rest) < 3 {
			fatal("usage: srv config set <profile> <key> <value>")
		}
		prof := rest[0]
		key := rest[1]
		value := strings.Join(rest[2:], " ")
		p, ok := cfg.Profiles[prof]
		if !ok {
			fatal("error: profile %q not found.", prof)
		}
		applyProfileSet(p, key, value)
		if err := SaveConfig(cfg); err != nil {
			fatal("error: %v", err)
		}
		fmt.Printf("%s.%s = %s\n", prof, key, value)
		return 0
	}
	fatal("error: unknown config action %q", action)
	return 1
}

// applyProfileSet writes value into the profile field named by key. Bool
// strings ("true"/"false") and digit strings auto-convert.
func applyProfileSet(p *Profile, key, value string) {
	v := strings.ToLower(value)
	asBool := func() *bool {
		if v == "true" {
			return boolPtr(true)
		}
		return boolPtr(false)
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
	default:
		// unknown key -> store stringly
		if p.Extra == nil {
			p.Extra = map[string]any{}
		}
		p.Extra[key] = value
	}
}

func cmdUse(args []string, cfg *Config) int {
	if len(args) == 0 {
		sid, rec := TouchSession()
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
		return 0
	}
	a := args[0]
	if a == "--clear" || a == "-" || a == "-c" {
		sid, err := SetSessionProfile("")
		if err != nil {
			fatal("error: %v", err)
		}
		fmt.Printf("session %s: unpinned\n", sid)
		return 0
	}
	if _, ok := cfg.Profiles[a]; !ok {
		fatal("error: profile %q not found. Run `srv config list`.", a)
	}
	sid, err := SetSessionProfile(a)
	if err != nil {
		fatal("error: %v", err)
	}
	cwd := cfg.Profiles[a].GetDefaultCwd()
	if c := GetCwd(a, cfg.Profiles[a]); c != "" {
		cwd = c
	}
	fmt.Printf("session %s: pinned to %q  (cwd: %s)\n", sid, a, cwd)
	return 0
}

func cmdCd(path string, cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	newCwd, err := changeRemoteCwd(name, profile, path)
	if err != nil {
		printDiagError(err, profile)
		return 1
	}
	fmt.Println(newCwd)
	return 0
}

func cmdPwd(cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Println(GetCwd(name, profile))
	return 0
}

func cmdStatus(cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	sid, rec := TouchSession()
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
	return 0
}

func cmdShell(cfg *Config, profileOverride string) int {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	cwd := GetCwd(name, profile)
	c, err := Dial(profile)
	if err != nil {
		printDiagError(err, profile)
		return 255
	}
	defer c.Close()
	rc, err := c.Shell(cwd)
	if err != nil {
		printDiagError(err, profile)
	}
	return rc
}

func cmdRun(args []string, cfg *Config, profileOverride string, tty bool) int {
	if len(args) == 0 {
		fatal("error: nothing to run.")
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	cmd := strings.Join(args, " ")
	cwd := GetCwd(name, profile)
	return runRemoteStream(profile, cwd, cmd, tty)
}

func cmdPush(args []string, cfg *Config, profileOverride string) int {
	args, recursive := stripRecursive(args)
	if len(args) == 0 {
		fatal("usage: srv push <local> [<remote>] [-r]")
	}
	local := args[0]
	if _, err := os.Stat(local); err != nil {
		fatal("error: local path does not exist: %s", local)
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	cwd := GetCwd(name, profile)
	remote := ""
	if len(args) > 1 {
		remote = args[1]
	} else {
		remote = baseName(local)
	}
	abs := resolveRemotePath(remote, cwd)
	rc, err := pushPath(profile, local, abs, recursive)
	if err != nil {
		printDiagError(err, profile)
	}
	return rc
}

func cmdPull(args []string, cfg *Config, profileOverride string) int {
	args, recursive := stripRecursive(args)
	if len(args) == 0 {
		fatal("usage: srv pull <remote> [<local>] [-r]")
	}
	remote := args[0]
	local := "."
	if len(args) > 1 {
		local = args[1]
	}
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}
	cwd := GetCwd(name, profile)
	abs := resolveRemotePath(remote, cwd)
	rc, err := pullPath(profile, abs, local, recursive)
	if err != nil {
		printDiagError(err, profile)
	}
	return rc
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

func cmdSessions(args []string) int {
	action := "list"
	if len(args) > 0 {
		action = args[0]
	}
	current := SessionID()
	s := loadSessionsFile()
	switch action {
	case "list":
		if len(s.Sessions) == 0 {
			fmt.Println("(no sessions)")
			return 0
		}
		ids := make([]string, 0, len(s.Sessions))
		for id := range s.Sessions {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			rec := s.Sessions[id]
			mark := "  "
			if id == current {
				mark = "> "
			}
			alive := "dead"
			if PidAlive(id) {
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
		return 0
	case "show":
		_, _ = TouchSession()
		s = loadSessionsFile()
		rec := s.Sessions[current]
		out := map[string]*SessionRecord{current: rec}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return 0
	case "clear":
		if _, ok := s.Sessions[current]; ok {
			delete(s.Sessions, current)
			_ = writeSessionsFile(s)
			fmt.Printf("cleared session %s\n", current)
		} else {
			fmt.Printf("session %s has no record\n", current)
		}
		return 0
	case "prune":
		before := len(s.Sessions)
		dead := 0
		for id := range s.Sessions {
			if !PidAlive(id) {
				delete(s.Sessions, id)
				dead++
			}
		}
		_ = writeSessionsFile(s)
		fmt.Printf("pruned %d/%d sessions (%d remaining)\n", dead, before, len(s.Sessions))
		return 0
	}
	fatal("error: unknown sessions action %q", action)
	return 1
}
