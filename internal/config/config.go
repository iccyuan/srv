package config

import (
	"encoding/json"
	"fmt"
	"os"
	"srv/internal/project"
	"srv/internal/session"
	"strings"
	"time"

	"srv/internal/i18n"
	"srv/internal/srvutil"
)

// Path helpers (Dir / Config / Sessions / Jobs) live in
// srv/internal/srvpath now -- everything that needs to know where
// srv keeps its files imports srvpath directly rather than chaining
// through package main.

// Profile mirrors the Python profile dict. Keys with omitempty/zero-value
// semantics:
//   - Multiplex/Compression default to true if not present (handled by
//     accessor methods)
//   - Most ints/strings have sensible zero defaults applied at use time
type Profile struct {
	Host              string            `json:"host"`
	User              string            `json:"user,omitempty"`
	Port              int               `json:"port,omitempty"`
	IdentityFile      string            `json:"identity_file,omitempty"`
	DefaultCwd        string            `json:"default_cwd,omitempty"`
	Multiplex         *bool             `json:"multiplex,omitempty"`
	Compression       *bool             `json:"compression,omitempty"`
	ConnectTimeout    int               `json:"connect_timeout,omitempty"`
	KeepaliveInterval int               `json:"keepalive_interval,omitempty"`
	KeepaliveCount    int               `json:"keepalive_count,omitempty"`
	ControlPersist    string            `json:"control_persist,omitempty"`
	SyncRoot          string            `json:"sync_root,omitempty"`
	SyncExclude       []string          `json:"sync_exclude,omitempty"`
	SshOptions        []string          `json:"ssh_options,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	// Jump (ProxyJump) -- one or more bastion hops dialed in order before
	// the final target. Each entry is "[user@]host[:port]" plus an
	// optional per-hop identity_file. Two JSON forms are accepted on
	// load and the simpler one round-trips on save:
	//
	//	"jump": ["userA@10.0.0.1:22"]                       // string form
	//	"jump": [{"spec":"userA@10.0.0.1","identity_file":"~/.ssh/keyA"}]
	//
	// When a hop has its own identity_file, the SSH agent (if any) plus
	// THAT key are used for the hop's handshake -- the profile's
	// identity_file is bypassed for that hop only. Hops without their
	// own key fall back to the same agent + profile.identity_file +
	// default key chain as the final target.
	Jump []JumpHop `json:"jump,omitempty"`
	// CompressSync controls whether `srv sync` gzips the tar stream over
	// the wire. nil = default true. ~70% size reduction for code, single-
	// digit ms CPU on the hot path.
	CompressSync *bool `json:"compress_sync,omitempty"`
	// DialAttempts: how many times to attempt the initial TCP dial / SSH
	// handshake before giving up. Default 1 (no retry, current behavior).
	// Set to 3-5 on flaky networks where the first SYN sometimes drops.
	// Auth and host-key errors never retry -- another attempt won't change
	// the answer.
	DialAttempts int `json:"dial_attempts,omitempty"`
	// DialBackoff: initial wait between dial retries; doubles each attempt
	// up to a 30s cap. Default "500ms". Parsed via time.ParseDuration so
	// "1s" / "200ms" / "2s500ms" all work.
	DialBackoff string `json:"dial_backoff,omitempty"`
	// AgentForwarding=true makes srv request ssh-agent forwarding for
	// the interactive run paths (`srv -t <cmd>`, `srv shell`). Local
	// SSH_AUTH_SOCK must be set for forwarding to actually function;
	// the flag is a no-op when it isn't. Doesn't apply to non-TTY
	// `srv <cmd>` or MCP paths because those rarely benefit and the
	// per-session round-trip cost adds up.
	AgentForwarding *bool `json:"agent_forwarding,omitempty"`
	// SSH crypto preference lists. Each is a comma-separated list of
	// algorithm names accepted by golang.org/x/crypto/ssh. Empty/nil
	// leaves the library default in place. Set when the negotiated
	// default is suboptimal for your hardware:
	//
	//	x86-64 with AES-NI:  ciphers: ["aes128-gcm@openssh.com"]
	//	ARM without AES-NI: ciphers: ["chacha20-poly1305@openssh.com"]
	//
	// MACs is ignored for AEAD ciphers (GCM / chacha20-poly1305 carry
	// authentication inline). Set it only when also pinning a non-AEAD
	// cipher like aes128-ctr (rare).
	Ciphers           []string `json:"ciphers,omitempty"`
	MACs              []string `json:"macs,omitempty"`
	KeyExchanges      []string `json:"key_exchanges,omitempty"`
	HostKeyAlgorithms []string `json:"host_key_algorithms,omitempty"`
	// CompressStreams=true wraps every captured remote command's stdout
	// in `| gzip -c -1` before it crosses the wire, and decompresses
	// client-side. Stays off by default because the CPU cost is a real
	// hit on fast LAN links and the bandwidth saving is invisible. Turn
	// on for cross-region / mobile-tethered links where `srv ls -R /`
	// or `cat large-log` would otherwise drag.
	CompressStreams *bool `json:"compress_streams,omitempty"`
	// Autoconnect=true asks the daemon to dial this profile into its
	// connection pool at startup, before any user request arrives. The
	// usual lazy-dial pattern pays the SSH handshake (~200-800ms) on
	// the very first call after a fresh daemon boot; pre-warm flips
	// that to 0-RTT for the profiles users actually live in. Failures
	// during pre-warm are logged but never block daemon startup -- a
	// down host shouldn't keep cd / ls / run from working against
	// every other profile.
	Autoconnect *bool `json:"autoconnect,omitempty"`
	// PoolSize caps the number of concurrent SSH connections the daemon
	// will keep open for this profile. Default 4 when unset -- a small
	// pool fills more of the underlying TCP bandwidth-delay product than
	// a single connection's SSH window allows and absorbs parallel MCP
	// storms / big sync trees / busy `srv ui` dashboards without any
	// manual tuning. Set 1 for the old single-connection behaviour;
	// crank to 8 for very high concurrency. Hard-clamped to [1,16] at
	// use time so a stray big number can't exhaust local fd or sshd's
	// MaxStartups budget.
	PoolSize int `json:"pool_size,omitempty"`
	// Proxy routes the SSH TCP dial through an HTTP-CONNECT or SOCKS5
	// proxy. URL forms:
	//
	//   socks5://[user:pass@]host:port    RFC 1928 + 1929
	//   http://[user:pass@]host:port      HTTP CONNECT + Basic auth
	//
	// Applies to the FIRST TCP dial only: when combined with Jump
	// (ProxyJump), the first hop reaches its destination through this
	// proxy and subsequent hops travel over the SSH channel chain. The
	// proxy is bypassed for direct daemon-RPC sockets and SFTP-on-
	// existing-conn calls since those don't open new TCP sessions.
	//
	// Empty string means no proxy (the historical behaviour and
	// default). Failures during the proxy handshake surface as dial
	// errors -- they don't fall back to direct connect, because that
	// would defeat a corporate egress policy that blocks the bypass.
	Proxy string `json:"proxy,omitempty"`
	// Inherits names another profile to inherit field defaults from.
	// Resolution is recursive (A inherits B inherits C); cycles are
	// detected and broken (the cycle's edge stays as-is, no infinite
	// recursion). Merge rule is "child wins when child has a non-zero
	// value":
	//
	//   - Strings / ints: child wins iff its value is non-empty / non-zero.
	//   - *bool (Multiplex, Compression, etc): child wins iff non-nil.
	//   - Slices ([]string, []JumpHop, ssh_options, ...): child wins iff
	//     non-nil. Set explicitly to `[]` to clear an inherited list.
	//   - map[string]string (Env): merged key-by-key, child wins on key
	//     collisions. This is the only field where parent and child
	//     contribute additively (the common pattern is "parent declares
	//     base env, child adds/overrides a few").
	//
	// IMPORTANT footgun: inheritance is resolved at Load() into the
	// in-memory Profile. If you Save() a child profile after that, the
	// merged values get written back into the JSON and the child stops
	// tracking parent's future changes. `srv config edit` and
	// `srv config set` both go through Save. Prefer editing config.json
	// directly when you want a child to stay symbolically linked to
	// its parent.
	Inherits string `json:"inherits,omitempty"`
	// Platform is an optional override for the remote operating system
	// kind. Accepted values: "linux", "darwin", "other" (BSD/illumos/
	// etc), or "" (unset, auto-detect). When unset, callers go
	// through remote.GetPlatform which probes `uname -s` once and
	// caches under ~/.srv/cache/platform-<name>.txt for 24h.
	//
	// Set explicitly to skip auto-detection -- useful for stripped-
	// down remotes where the probe is slow / unreliable, or to force
	// "treat as X" while a host migration is in progress. The escape
	// hatch wins absolutely; auto-detect is the default.
	Platform string `json:"platform,omitempty"`
	// Free-form bag for unknown keys forwarded from older Python configs.
	Extra map[string]any `json:"-"`
	// Name is the profile's lookup key in Config.Profiles. Populated by
	// ResolveProfile so deeper layers can include it in diagnostics
	// without threading the name through every signature. NOT serialized.
	Name string `json:"-"`
	// JumpResolved is the post-resolution form of Jump. Populated by
	// Config.ResolveJumps (called from Load) after expanding any bare
	// profile-name entries in Jump into concrete "[user@]host[:port]"
	// hops + propagated identity_file. The dial path iterates this slice
	// when non-nil; the persisted Jump keeps the symbolic form so save
	// round-trips don't lose the reference. NOT serialized.
	JumpResolved []JumpHop `json:"-"`
}

// JumpHop is one entry in a profile's ProxyJump chain. Spec is the
// "[user@]host[:port]" string parsed by sshx.parseHostSpec. IdentityFile
// is optional; when set, that key (rather than the parent profile's
// identity_file) is used for the hop's SSH handshake.
type JumpHop struct {
	Spec         string `json:"spec"`
	IdentityFile string `json:"identity_file,omitempty"`
}

// UnmarshalJSON accepts either a bare string ("user@host:22") or the
// full object form ({"spec":"...","identity_file":"..."}). The bare
// string form is the historical layout -- we keep accepting it so
// existing configs don't need migration when this field grew an
// optional per-hop key.
func (h *JumpHop) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		h.Spec = s
		h.IdentityFile = ""
		return nil
	}
	type alias JumpHop
	return json.Unmarshal(b, (*alias)(h))
}

// MarshalJSON emits the bare string form when IdentityFile is empty so
// pre-existing configs round-trip unchanged. Hops with their own key
// fall back to the object form.
func (h JumpHop) MarshalJSON() ([]byte, error) {
	if h.IdentityFile == "" {
		return json.Marshal(h.Spec)
	}
	type alias JumpHop
	return json.Marshal(alias(h))
}

// looksLikeProfileName reports whether a jump entry's Spec is a bare
// profile-name reference (e.g. "bastion") vs. an SSH host literal
// ("user@10.0.0.1:22"). Profile names cannot contain '@' or ':', so the
// absence of both is a reliable signal -- and we additionally require
// the name to exist in cfg.Profiles before expanding, so a typo in a
// hostname won't accidentally swallow a literal that just happens to
// be punctuation-free.
func looksLikeProfileName(spec string) bool {
	return spec != "" &&
		!strings.ContainsAny(spec, "@:")
}

// ResolveJumps populates each profile's JumpResolved by expanding any
// bare profile-name references in Jump into concrete hops. When a hop
// references profile A, A's own Jump is recursively prepended so the
// full chain to A is honored before A itself is dialed. Cycles (A→B→A)
// are broken: the offending entry resolves to a bare literal of the
// name so the dial surfaces a clear failure instead of looping forever.
//
// Per-hop IdentityFile takes precedence over the referenced profile's
// identity_file -- this lets callers override A's default key for one
// hop without touching A's profile.
func (c *Config) ResolveJumps() {
	if c == nil {
		return
	}
	for name, p := range c.Profiles {
		if p == nil {
			continue
		}
		p.Name = name
		p.JumpResolved = c.resolveJumpChain(p, map[string]bool{name: true})
	}
}

// resolveJumpChain walks `p.Jump` and returns the flattened, concrete
// list of hops to dial before reaching `p` itself. `visiting` carries
// every profile name on the current resolution stack so a cycle short-
// circuits with the literal name (and dial then fails with a helpful
// "profile not found" / parse error rather than an infinite recursion).
func (c *Config) resolveJumpChain(p *Profile, visiting map[string]bool) []JumpHop {
	if p == nil || len(p.Jump) == 0 {
		return nil
	}
	out := make([]JumpHop, 0, len(p.Jump))
	for _, hop := range p.Jump {
		if !looksLikeProfileName(hop.Spec) {
			out = append(out, hop)
			continue
		}
		ref, ok := c.Profiles[hop.Spec]
		if !ok {
			// Unknown name -- leave it as-is. Dial will surface a
			// "lookup hostname failed" the user can act on; we do
			// NOT silently drop it.
			out = append(out, hop)
			continue
		}
		if visiting[hop.Spec] {
			// Cycle. Keep the literal so dial fails loudly rather
			// than this function spinning.
			out = append(out, hop)
			continue
		}
		visiting[hop.Spec] = true
		// Recursively prepend A's own jump chain.
		out = append(out, c.resolveJumpChain(ref, visiting)...)
		// Then add A itself as a concrete hop.
		out = append(out, jumpHopFromProfile(ref, hop.IdentityFile))
		delete(visiting, hop.Spec)
	}
	return out
}

// ResolveInherits walks every profile and folds in the fields of the
// profile named in `Inherits` (recursively up the chain). Mutates the
// in-memory profile structs; intended to run once at Load() time
// BEFORE ResolveJumps (so an inherited Jump chain gets the same name-
// expansion treatment as a directly-declared one). Cycles
// (`A inherits B inherits A`) short-circuit at the second visit; the
// inheriting edge silently stops there rather than recursing forever.
//
// Per-field merge rules live in mergeProfileFromParent. Documented in
// the Profile.Inherits doc comment because they're part of the user-
// facing contract, not just an internal detail.
func (c *Config) ResolveInherits() {
	if c == nil {
		return
	}
	for name, p := range c.Profiles {
		if p == nil {
			continue
		}
		p.Name = name
		c.applyInheritance(p, map[string]bool{name: true})
	}
}

// applyInheritance walks `p.Inherits` (and grandparent links) folding
// each ancestor's fields into `p` via mergeProfileFromParent. The
// visiting set carries every name on the current chain so a cycle
// halts at the repeated edge. Missing parents are silently skipped --
// dial-time errors against the unresolved field surface them clearly
// enough (e.g. "no identity_file") and we don't want Load() to fail
// on a typo in one profile.
func (c *Config) applyInheritance(p *Profile, visiting map[string]bool) {
	if p == nil || p.Inherits == "" {
		return
	}
	parent, ok := c.Profiles[p.Inherits]
	if !ok || parent == nil {
		return
	}
	if visiting[p.Inherits] {
		return
	}
	visiting[p.Inherits] = true
	// Resolve grandparent's inheritance first so `parent` is itself
	// fully merged before we read from it. Operates on a temporary
	// copy because mutating the shared parent would leak the merge
	// across siblings that inherit from it -- inheritance must be a
	// child-side rewrite, never a parent-side mutation.
	effectiveParent := *parent
	c.applyInheritance(&effectiveParent, visiting)
	mergeProfileFromParent(p, &effectiveParent)
}

// mergeProfileFromParent applies the documented merge rules from
// Profile.Inherits's doc comment: child non-zero wins, slices replace
// (not append), Env maps merge key-by-key with child winning. Care is
// taken NOT to copy Name / Inherits / JumpResolved / Extra fields
// from parent -- those are either bookkeeping (Name, JumpResolved) or
// would create surprising chained inheritance (Inherits).
func mergeProfileFromParent(child, parent *Profile) {
	if child == nil || parent == nil {
		return
	}
	if child.Host == "" {
		child.Host = parent.Host
	}
	if child.User == "" {
		child.User = parent.User
	}
	if child.Port == 0 {
		child.Port = parent.Port
	}
	if child.IdentityFile == "" {
		child.IdentityFile = parent.IdentityFile
	}
	if child.DefaultCwd == "" {
		child.DefaultCwd = parent.DefaultCwd
	}
	if child.Multiplex == nil {
		child.Multiplex = parent.Multiplex
	}
	if child.Compression == nil {
		child.Compression = parent.Compression
	}
	if child.ConnectTimeout == 0 {
		child.ConnectTimeout = parent.ConnectTimeout
	}
	if child.KeepaliveInterval == 0 {
		child.KeepaliveInterval = parent.KeepaliveInterval
	}
	if child.KeepaliveCount == 0 {
		child.KeepaliveCount = parent.KeepaliveCount
	}
	if child.ControlPersist == "" {
		child.ControlPersist = parent.ControlPersist
	}
	if child.SyncRoot == "" {
		child.SyncRoot = parent.SyncRoot
	}
	if child.SyncExclude == nil {
		child.SyncExclude = parent.SyncExclude
	}
	if child.SshOptions == nil {
		child.SshOptions = parent.SshOptions
	}
	if child.Jump == nil {
		child.Jump = parent.Jump
	}
	if child.CompressSync == nil {
		child.CompressSync = parent.CompressSync
	}
	if child.DialAttempts == 0 {
		child.DialAttempts = parent.DialAttempts
	}
	if child.DialBackoff == "" {
		child.DialBackoff = parent.DialBackoff
	}
	if child.AgentForwarding == nil {
		child.AgentForwarding = parent.AgentForwarding
	}
	if child.Ciphers == nil {
		child.Ciphers = parent.Ciphers
	}
	if child.MACs == nil {
		child.MACs = parent.MACs
	}
	if child.KeyExchanges == nil {
		child.KeyExchanges = parent.KeyExchanges
	}
	if child.HostKeyAlgorithms == nil {
		child.HostKeyAlgorithms = parent.HostKeyAlgorithms
	}
	if child.CompressStreams == nil {
		child.CompressStreams = parent.CompressStreams
	}
	if child.Autoconnect == nil {
		child.Autoconnect = parent.Autoconnect
	}
	if child.PoolSize == 0 {
		child.PoolSize = parent.PoolSize
	}
	if child.Proxy == "" {
		child.Proxy = parent.Proxy
	}
	if child.Platform == "" {
		child.Platform = parent.Platform
	}
	// Env: merge key-by-key, child wins on collisions. This is the
	// one field where the additive semantics actually match how
	// people structure shared base + per-host overrides. Replicating
	// the same "child wins if non-nil" rule we use for slices would
	// force the child to restate every parent var.
	if len(parent.Env) > 0 {
		if child.Env == nil {
			child.Env = map[string]string{}
		}
		for k, v := range parent.Env {
			if _, present := child.Env[k]; !present {
				child.Env[k] = v
			}
		}
	}
}

// jumpHopFromProfile converts a referenced profile into a concrete
// JumpHop. `override` wins over the referenced profile's identity_file
// when set, so callers can pin a different key for this one hop.
func jumpHopFromProfile(p *Profile, override string) JumpHop {
	spec := p.Host
	if p.User != "" {
		spec = p.User + "@" + spec
	}
	if p.Port != 0 && p.Port != 22 {
		spec = spec + ":" + srvutil.IntToStr(p.Port)
	}
	id := override
	if id == "" {
		id = p.IdentityFile
	}
	return JumpHop{Spec: spec, IdentityFile: id}
}

func (p *Profile) GetPort() int {
	if p.Port == 0 {
		return 22
	}
	return p.Port
}

func (p *Profile) GetConnectTimeout() int {
	if p.ConnectTimeout == 0 {
		return 10
	}
	return p.ConnectTimeout
}

func (p *Profile) GetKeepaliveInterval() int {
	if p.KeepaliveInterval == 0 {
		return 30
	}
	return p.KeepaliveInterval
}

func (p *Profile) GetKeepaliveCount() int {
	if p.KeepaliveCount == 0 {
		return 3
	}
	return p.KeepaliveCount
}

func (p *Profile) GetCompression() bool {
	if p.Compression == nil {
		return true
	}
	return *p.Compression
}

func (p *Profile) GetCompressSync() bool {
	if p.CompressSync == nil {
		return true
	}
	return *p.CompressSync
}

// GetCompressStreams reports whether captured remote commands should
// gzip their stdout over the wire. Default false. See the field doc
// for when it pays off and when it costs.
func (p *Profile) GetCompressStreams() bool {
	return p.CompressStreams != nil && *p.CompressStreams
}

// GetAutoconnect reports whether the daemon should pre-warm this
// profile's SSH connection at startup. Default false -- the lazy
// dial pattern is the right tradeoff for profiles touched once a
// week; flip it on for the one or two profiles you use constantly.
func (p *Profile) GetAutoconnect() bool {
	return p.Autoconnect != nil && *p.Autoconnect
}

// GetPoolSize returns the clamped concurrent-connection cap. Default 4
// when unset (or <1): a small pool absorbs parallel MCP / sync / `srv
// ui` load without the user tuning anything, and fills more of the TCP
// bandwidth-delay product than a single SSH window allows. Values
// outside [1,16] are clamped silently because a stray 0 or 999 in the
// JSON would otherwise be hard to diagnose. Set pool_size:1 explicitly
// for the old single-connection behaviour.
func (p *Profile) GetPoolSize() int {
	n := p.PoolSize
	if n < 1 {
		return 4
	}
	if n > 16 {
		return 16
	}
	return n
}

func (p *Profile) GetDefaultCwd() string {
	if p.DefaultCwd == "" {
		return "~"
	}
	return p.DefaultCwd
}

// GetAgentForwarding reports whether `srv -t` / `srv shell` should
// request ssh-agent forwarding for this profile. Defaults to false so
// existing profiles see no behavior change.
func (p *Profile) GetAgentForwarding() bool {
	return p.AgentForwarding != nil && *p.AgentForwarding
}

func (p *Profile) GetDialAttempts() int {
	if p.DialAttempts < 1 {
		// Profiles with a ProxyJump chain have more failure points
		// (every hop's TCP + SSH handshake adds an opportunity for a
		// transient timeout / sshd MaxStartups burst / mid-path NAT
		// blip). A single retry costs ~500ms-2s and dramatically
		// reduces visible failures against flaky bastion paths. The
		// default stays 1 for direct profiles -- a healthy LAN box
		// shouldn't pay even one retry. Explicit dial_attempts in the
		// profile overrides this auto-bump.
		if len(p.Jump) > 0 {
			return 2
		}
		return 1
	}
	return p.DialAttempts
}

func (p *Profile) GetDialBackoff() time.Duration {
	if p.DialBackoff == "" {
		return 500 * time.Millisecond
	}
	d, err := time.ParseDuration(p.DialBackoff)
	if err != nil || d < 0 {
		return 500 * time.Millisecond
	}
	return d
}

// SchemaVersion moved to srv/internal/srvutil.SchemaVersion. Existing
// references in this file (and config.json `_version` handling) read
// srvutil.SchemaVersion directly.

// Config maps to ~/.srv/config.json.
//
// Top-level fields beyond DefaultProfile are user-tunable globals that
// don't belong on a single profile -- they affect srv's local behavior
// regardless of which server you're talking to. Set via
// `srv settings <key> <value>`. nil pointer fields distinguish
// "user hasn't said" from "user said false".
type Config struct {
	Version        int    `json:"_version,omitempty"`
	DefaultProfile string `json:"default_profile"`
	// Hints toggles the "did you mean / typo" command hint emitter.
	// nil means default-on; *bool false means explicitly disabled.
	// Env var SRV_HINTS=0 or the --no-hints flag also disables.
	Hints *bool `json:"hints,omitempty"`
	// Lang controls UI language for help text + high-traffic error
	// strings. "" or "auto" = environment detection (SRV_LANG,
	// LC_ALL, LC_MESSAGES, LANG; falls back to English). "en" / "zh"
	// pin explicitly. Unknown values fall back to English.
	Lang     string              `json:"lang,omitempty"`
	Profiles map[string]*Profile `json:"profiles"`
	// Groups names a list of profiles that fan-out commands (`srv -G
	// <group> <cmd>`) target in parallel. Keys are group names, values
	// are ordered profile names. Empty groups are valid in the on-disk
	// config but error at use time -- a half-defined group is easier to
	// debug than a silent no-op.
	Groups map[string][]string `json:"groups,omitempty"`
	// Tunnels is the catalog of named, persistable tunnels. Bring one up
	// with `srv tunnel up <name>` (runs inside the daemon, survives the
	// CLI exit) and tear down with `srv tunnel down`. Autostart entries
	// come up automatically when the daemon starts.
	Tunnels map[string]*TunnelDef `json:"tunnels,omitempty"`
	// Hooks fires local shell commands on srv's command lifecycle events
	// (pre-cd, post-cd, pre-sync, post-sync, pre-run, post-run, pre-push,
	// post-push, pre-pull, post-pull). Keys are event names, values are
	// commands run in order via the user's shell with SRV_* env vars.
	Hooks map[string][]string `json:"hooks,omitempty"`
	// JobNotify configures the daemon's "job finished" notifier (local
	// OS toast + optional webhook). nil = disabled. Toggled via
	// `srv jobs notify on|off`.
	JobNotify *JobNotifyConfig `json:"job_notify,omitempty"`
	// Guard customises the MCP high-risk-pattern allowlist / blocklist.
	// nil = use the built-in default pattern set. Extra rules are
	// appended on top of the defaults; allow patterns short-circuit
	// any deny match for the same command (escape hatch for benign
	// uses like `mkfs.btrfs --help`).
	Guard *GuardConfig `json:"guard,omitempty"`
	// Recipes are named multi-step playbooks: a sequence of remote
	// commands with positional ($1..$N) and named (${KEY}) parameter
	// substitution. Run via `srv recipe run <name> [args]`.
	Recipes map[string]*Recipe `json:"recipes,omitempty"`
}

// Recipe is one named playbook. Steps run in order against the
// profile resolved at run time (or the recipe's pinned profile when
// set). A non-zero exit aborts the rest of the steps unless
// `IgnoreErrors=true`.
type Recipe struct {
	Description  string   `json:"description,omitempty"`
	Profile      string   `json:"profile,omitempty"`       // optional pin
	Steps        []string `json:"steps"`                   // shell-quoted commands
	IgnoreErrors bool     `json:"ignore_errors,omitempty"` // continue past failures
}

// GuardConfig overrides / extends the MCP guard's pattern set.
// DisableDefaults=true skips the built-in patterns entirely so users
// can replace the policy from scratch; left false, user rules append.
type GuardConfig struct {
	DisableDefaults bool `json:"disable_defaults,omitempty"`
	// GlobalOff is the machine-wide guard switch, consulted only when
	// there's no SRV_GUARD env and no per-session `srv guard on|off`
	// decision. nil = unset (built-in default: guard ON). *true = OFF
	// everywhere -- crucially incl. the MCP server, whose ppid-derived
	// session id never matches the user's interactive shell, so a
	// plain `srv guard off` can't reach it. *false = explicitly ON.
	// Set via `srv guard off --global` / `srv guard on --global`.
	GlobalOff *bool       `json:"global_off,omitempty"`
	Rules     []GuardRule `json:"rules,omitempty"`
	// Allow lists regex patterns that *unblock* commands matching the
	// rule set. Useful for "allow `rm -rf ./tmp/...` from this profile
	// only" without disabling the whole pattern.
	Allow []string `json:"allow,omitempty"`
}

// GuardRule is one named pattern. Name is shown to the model in the
// rejection text so it can choose a different approach without trial
// and error.
type GuardRule struct {
	Name    string `json:"name"`
	Pattern string `json:"pattern"`
}

// GuardActive resolves the EFFECTIVE high-risk-op guard state with
// full precedence:
//
//  1. SRV_GUARD env            ("on"/"off")        -- definitive
//  2. per-session srv guard    (Record.Guard)      -- this shell
//  3. global config GlobalOff  (srv guard --global)-- machine-wide
//  4. built-in default                              -- ON
//
// This is the single source of truth. session.GuardOn() is only the
// env+session slice (layers 1,2,4) -- it can't see config because
// config imports session and the reverse would cycle. Every guard
// consumer that has a *config.Config (the MCP server does, on every
// call) must go through here so `srv guard off --global` actually
// reaches the MCP subprocess, whose ppid-derived session never
// matches the user's interactive shell.
func (c *Config) GuardActive() bool {
	switch session.GuardPref() {
	case session.GuardEnabled:
		return true
	case session.GuardDisabled:
		return false
	}
	if c != nil && c.Guard != nil && c.Guard.GlobalOff != nil {
		return !*c.Guard.GlobalOff
	}
	return true
}

// JobNotifyConfig captures the user's notification preferences for
// detached job completion. Daemon-driven so notifications fire even
// when the CLI that started the job has long since exited.
type JobNotifyConfig struct {
	// Local controls OS-native toast notifications. On macOS this uses
	// osascript, Linux uses notify-send, Windows uses a tiny PowerShell
	// MessageBox via the daemon process. Best-effort: missing tool -> log
	// line + skip rather than failing the job.
	Local bool `json:"local,omitempty"`
	// Webhook is a URL the daemon POSTs to on each completion. Empty
	// disables. Payload is a JSON object: { id, profile, cmd, pid, log,
	// started, finished }. 10-second timeout; failure is logged once.
	Webhook string `json:"webhook,omitempty"`
}

// TunnelDef is the saved-on-disk description of one named tunnel. The
// runtime state (active listener, started time, etc.) lives in the
// daemon and is queried via the `tunnel_list` daemon op.
type TunnelDef struct {
	// Type is "local" (default; like `ssh -L`) or "remote" (`ssh -R`).
	Type string `json:"type"`
	// Spec uses the same forms as the one-shot CLI: "8080",
	// "8080:9090", or "8080:host:9090".
	Spec string `json:"spec"`
	// Profile name to dial. Empty falls back to ResolveProfile rules at
	// up-time so $SRV_PROFILE / `.srv-project` still apply.
	Profile string `json:"profile,omitempty"`
	// Autostart=true brings the tunnel up automatically when the daemon
	// starts (typical for "always-on" things like a db port-forward).
	Autostart bool `json:"autostart,omitempty"`
	// OnDemand=true makes the daemon open the local listener but defer
	// the SSH dial until the first client connects. Saves SSH channels
	// for tunnels that stay defined "just in case" but rarely see
	// traffic. Local-direction only -- reverse tunnels need the SSH
	// session up to set up the remote listener and can't be lazy.
	OnDemand bool `json:"on_demand,omitempty"`
	// Independent=true makes `srv tunnel up <name>` spawn a dedicated
	// `srv _tunnel_run` subprocess to host this tunnel instead of
	// parking it inside the long-lived daemon. The trade is:
	//   - Independent: daemon restarts don't disturb the tunnel, but
	//     each independent tunnel costs its own SSH connection +
	//     ~20MB Go runtime; status flows through ~/.srv/tunnels/<name>.json
	//     instead of the daemon's tunnel_list RPC.
	//   - Daemon-hosted (default): tunnels share the daemon's SSH
	//     pool and lifecycle; restarting the daemon for a config
	//     change tears every tunnel down.
	// Default is daemon-hosted (the historical behaviour); flip this
	// on for tunnels you treat as critical infrastructure (db
	// forwards, observability bridges) that mustn't die during a
	// daemon restart.
	Independent bool `json:"independent,omitempty"`
}

// HintsEnabled reports whether typo / post-failure hints should fire.
// Config wins over env-default (true), and the --no-hints flag plus
// SRV_HINTS=0 are checked separately by the caller.
func (c *Config) HintsEnabled() bool {
	if c == nil || c.Hints == nil {
		return true
	}
	return *c.Hints
}

func New() *Config {
	return &Config{Profiles: map[string]*Profile{}}
}

// Load returns nil with nil error when the file doesn't exist yet.
func Load() (*Config, error) {
	data, err := os.ReadFile(srvutil.Config())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", srvutil.Config(), err)
	}
	cfg := New()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", srvutil.Config(), err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]*Profile{}
	}
	// Fold parent fields into children FIRST (so an inherited Jump
	// chain gets the same name-expansion treatment), then expand any
	// bare profile-name references in every profile's Jump chain
	// into concrete hops. Order matters: ResolveJumps reads the
	// post-inherit Jump field.
	cfg.ResolveInherits()
	cfg.ResolveJumps()
	srvutil.WarnIfNewerSchema(srvutil.Config(), cfg.Version)
	return cfg, nil
}

func Save(cfg *Config) error {
	cfg.Version = srvutil.SchemaVersion
	return srvutil.WriteJSONFile(srvutil.Config(), cfg)
}

// srvutil.WriteJSONFile / srvutil.WriteFileAtomic / srvutil.WarnIfNewerSchema / srvutil.SchemaVersion
// live in srv/internal/srvio now. Use srvutil.WriteJSONFile etc.

// Resolve picks the active profile by precedence:
// override > session pin > $SRV_PROFILE > .srv-project pin > config default.
//
// The `.srv-project` step slots in just before the global default so a
// repo-level pin wins over "whatever profile happens to be default
// today" but still respects the user's session-scoped or env-scoped
// override. See findProjectFile for the lookup rules.
func Resolve(cfg *Config, override string) (string, *Profile, error) {
	name := override
	if name == "" {
		_, rec := session.Touch()
		if rec.Profile != nil {
			name = *rec.Profile
		}
	}
	if name == "" {
		name = os.Getenv("SRV_PROFILE")
	}
	if name == "" {
		if pf := project.Resolve(); pf != nil && pf.Profile != "" {
			name = pf.Profile
		}
	}
	if name == "" {
		name = cfg.DefaultProfile
	}
	if name == "" {
		return "", nil, fmt.Errorf("%s", i18n.T("err.no_profile"))
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return "", nil, fmt.Errorf("%s", i18n.T("err.profile_not_found", name))
	}
	p.Name = name
	return name, p, nil
}

// GetCwd returns the persisted cwd for (current session, profile),
// falling back through:
//
//	session-persisted cwd > $SRV_CWD > .srv-project cwd > profile.default_cwd
//
// $SRV_CWD predates the project file and stays higher: it's an explicit
// per-invocation override (e.g. set in an MCP server registration's env
// block), whereas .srv-project is a passive checked-in pin. The project
// file is a one-time setup that travels with the repo; the env override
// is what you reach for when one launch needs something different.
func GetCwd(profileName string, profile *Profile) string {
	_, rec := session.Touch()
	if cwd, ok := rec.Cwds[profileName]; ok && cwd != "" {
		return cwd
	}
	if env := os.Getenv("SRV_CWD"); env != "" {
		return env
	}
	if pf := project.Resolve(); pf != nil && pf.Cwd != "" {
		return pf.Cwd
	}
	return profile.GetDefaultCwd()
}

// SetCwd persists a new cwd for (current session, profile). Before
// overwriting, the previous value is captured into rec.PrevCwds so
// `srv cd -` can swap back to it (shell-style).
func SetCwd(profileName, cwd string) error {
	sid, rec := session.Touch()
	if rec.PrevCwds == nil {
		rec.PrevCwds = map[string]string{}
	}
	if old, ok := rec.Cwds[profileName]; ok && old != "" && old != cwd {
		rec.PrevCwds[profileName] = old
	}
	rec.Cwds[profileName] = cwd
	return session.SaveWith(sid, rec)
}

// GetPrevCwd returns the cwd this session was on before the most
// recent `srv cd`. Empty when no prior cwd has been recorded.
func GetPrevCwd(profileName string) string {
	_, rec := session.Touch()
	if rec.PrevCwds == nil {
		return ""
	}
	return rec.PrevCwds[profileName]
}

// SetSessionProfile pins (or clears with empty string) the session profile.
func SetSessionProfile(name string) (string, error) {
	sid, rec := session.Touch()
	if name == "" {
		rec.Profile = nil
	} else {
		rec.Profile = &name
	}
	return sid, session.SaveWith(sid, rec)
}
