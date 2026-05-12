package i18n

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// Tiny i18n: two languages, one map. Anything not in the table falls
// back to English. Designed to translate only what users actually read
// in flow (help text, fatal/usage strings, hint output) -- technical
// outputs like `srv check` diagnostics, daemon protocol fields, and MCP
// tool responses stay English so terminology doesn't drift.

type lang int

const (
	langEN lang = iota
	langZH
)

var (
	detectedLangOnce sync.Once
	detectedLangVal  lang

	// inMCPMode is set by callers via SetMCPMode. When true, T() always
	// returns English regardless of locale -- AI clients read tool
	// descriptions back into the model and any locale-dependent text
	// makes behaviour non-reproducible across machines.
	inMCPMode bool

	// configLangProvider lets package main register a lazy lookup
	// into Config.Lang without i18n having to depend on the config
	// package. Returns the configured language string ("en"/"zh"/"")
	// when available, "" when no config is present.
	configLangProvider = func() string { return "" }

	// platformLangProvider returns the OS-reported locale tag (e.g.
	// "zh-cn") on Windows, or "" elsewhere. Set by the
	// build-tagged platform_*.go file in this package.
	platformLangProvider = func() string { return "" }
)

// SetMCPMode pins the active language to English for the rest of the
// process when called with true. mcp_loop.go invokes it on startup.
func SetMCPMode(on bool) { inMCPMode = on }

// SetConfigLangProvider lets package main inject a lazy reader for
// Config.Lang so i18n can honour the user's pinned language without
// having a hard dependency on the config package. Provider may return
// "" to defer to env vars / platform locale.
func SetConfigLangProvider(fn func() string) {
	if fn != nil {
		configLangProvider = fn
	}
}

// currentLang returns the active UI language for the process.
// Detection runs at most once and caches the result.
//
// Resolution order (highest first):
//  1. inMCPMode -> always English. AI clients (Claude Code / Codex)
//     read tool descriptions and error messages and feed them back
//     into the model; locale-dependent text creates two problems:
//     (a) the model's learned patterns for srv-style tools assume
//     English, (b) the same prompt produces different model
//     behaviour on a 中文 vs en_US machine. Pin English under MCP
//     so behaviour is reproducible.
//  2. configLangProvider() ("en" / "zh"; "" or "auto" defers to env)
//  3. $SRV_LANG ("en" / "zh"; "auto" defers)
//  4. $LC_ALL / $LC_MESSAGES / $LANG (anything starting with "zh" ->
//     Chinese; else English)
//  5. Platform locale (Windows only, via platformLangProvider).
//  6. English fallback
func currentLang() lang {
	if inMCPMode {
		return langEN
	}
	detectedLangOnce.Do(func() {
		detectedLangVal = detectLang()
	})
	return detectedLangVal
}

func detectLang() lang {
	switch strings.ToLower(strings.TrimSpace(configLangProvider())) {
	case "en":
		return langEN
	case "zh":
		return langZH
	}
	if v := os.Getenv("SRV_LANG"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "en":
			return langEN
		case "zh":
			return langZH
		}
	}
	// POSIX locale envs. The first non-empty one wins; if it's not
	// zh* we treat the user as having explicitly opted into a
	// non-Chinese locale (English fallback) and skip the platform
	// probe.
	posixSet := false
	for _, k := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := os.Getenv(k); v != "" {
			posixSet = true
			if strings.HasPrefix(strings.ToLower(v), "zh") {
				return langZH
			}
			break
		}
	}
	if !posixSet {
		if strings.HasPrefix(platformLangProvider(), "zh") {
			return langZH
		}
	}
	return langEN
}

// T looks up the active-language template for key, applies fmt
// arguments, and returns the formatted string. Falls back to the
// English template when a key is missing in the active language. If
// even the English template is missing, returns the key itself --
// surfaces missing translations as obvious bugs in stderr.
func T(key string, args ...any) string {
	tmpl := messageFor(currentLang(), key)
	if len(args) == 0 {
		return tmpl
	}
	return fmt.Sprintf(tmpl, args...)
}

func messageFor(l lang, key string) string {
	if table, ok := messages[l]; ok {
		if msg, ok := table[key]; ok {
			return msg
		}
	}
	if l != langEN {
		if msg, ok := messages[langEN][key]; ok {
			return msg
		}
	}
	return key // missing translation; show the key
}

// messages holds the translation table. Add new keys with an English
// entry first; Chinese entries are optional and fall back automatically.
//
// Convention: keys use dotted lowercase namespaces:
//
//	help.full          full helpText output
//	err.<situation>    fatal/error strings the user reads
//	usage.<cmd>        per-subcommand usage line
//	hint.<situation>   hint emitter output
//	misc.<thing>       small one-offs
var messages = map[lang]map[string]string{
	langEN: {
		"help.full": helpEN,

		// fatal / error strings
		"err.no_profile":          "error: no profile selected. Run `srv init`, then `srv use <profile>` to pin one for this shell.",
		"err.profile_not_found":   "error: profile %q not found. Run `srv config list`.",
		"err.unknown_subcommand":  "error: unknown subcommand %q",
		"err.flag_requires_value": "error: %s requires a value.",
		"err.local_path_missing":  "error: local path does not exist: %s",
		"err.unknown_check_flag":  "error: unknown check flag %q",
		"err.unknown_sync_opt":    "error: unknown sync option %q",
		"err.sync_one_root":       "error: only one remote root accepted, got %v",
		"err.delete_requires_git": "error: --delete currently requires git mode",
		"err.config_action":       "error: unknown config action %q",
		"err.global_key_required": "error: srv config global <key> [<value> | --clear] (key required)",
		"err.global_unknown_key":  "error: unknown global key %q (supported: hints, lang, default_profile)",
		"err.global_lang_value":   "error: lang must be one of: en, zh, auto (got %q)",

		// usage lines
		"usage.config":      "usage: srv config <list|default|global|remove|show|set|edit> [args]",
		"usage.config_set":  "usage: srv config set <profile> <key> <value>",
		"usage.config_def":  "usage: srv config default <name>  (or no arg for interactive picker on a TTY)",
		"usage.config_rm":   "usage: srv config remove <name>",
		"usage.config_edit": "usage: srv config edit <profile>",
		"usage.push":        "usage: srv push <local> [<remote>] [-r]",
		"usage.pull":        "usage: srv pull <remote> [<local>] [-r]",
		"usage.tunnel":      "usage: srv tunnel <localPort>[:[<remoteHost>:]<remotePort>]",
		"usage.edit":        "usage: srv edit <remote_path>",
		"usage.open":        "usage: srv open <remote_file>",
		"usage.diff":        "usage: srv diff <local_file> [remote_file]",
		"usage.env_set":     "usage: srv env set <key> <value>",
		"usage.env_unset":   "usage: srv env unset <key>",
		"usage.env_other":   "usage: srv env [list|set|unset|clear]",

		// hint emitter
		"hint.typo_pre":  "srv: hint: %q looks like the local subcommand %q. Running on remote anyway.",
		"hint.typo_post": "srv: hint: %q isn't installed on the remote and looks like %q (a local subcommand). Try: srv %s",

		// misc
		"misc.no_profiles_run_init": "(no profiles configured -- run `srv init`)",
		"misc.global_lang_auto":     "(auto = environment detection)",
	},

	langZH: {
		"help.full": helpZH,

		// fatal / error strings
		"err.no_profile":          "error: 未选 profile。先 `srv init`,再 `srv use <profile>` pin 到本 shell。",
		"err.profile_not_found":   "error: profile %q 不存在。`srv config list` 看现有的。",
		"err.unknown_subcommand":  "error: 未知子命令 %q",
		"err.flag_requires_value": "error: %s 需要一个值。",
		"err.local_path_missing":  "error: 本地路径不存在:%s",
		"err.unknown_check_flag":  "error: check 不认识 flag %q",
		"err.unknown_sync_opt":    "error: sync 不认识 flag %q",
		"err.sync_one_root":       "error: 只接受一个远端根目录,收到 %v",
		"err.delete_requires_git": "error: --delete 目前只支持 git 模式",
		"err.config_action":       "error: config 子动作未知 %q",
		"err.global_key_required": "error: srv config global <key> [<value> | --clear](需要 key)",
		"err.global_unknown_key":  "error: 未知顶层配置 %q(支持:hints / lang / default_profile)",
		"err.global_lang_value":   "error: lang 取值只能是 en / zh / auto,收到 %q",

		// usage lines
		"usage.config":      "用法:srv config <list|default|global|remove|show|set|edit> [args]",
		"usage.config_set":  "用法:srv config set <profile> <key> <value>",
		"usage.config_def":  "用法:srv config default <name>  (无参在 TTY 下弹 ↑↓ 选择器)",
		"usage.config_rm":   "用法:srv config remove <name>",
		"usage.config_edit": "用法:srv config edit <profile>",
		"usage.push":        "用法:srv push <local> [<remote>] [-r]",
		"usage.pull":        "用法:srv pull <remote> [<local>] [-r]",
		"usage.tunnel":      "用法:srv tunnel <localPort>[:[<remoteHost>:]<remotePort>]",
		"usage.edit":        "用法:srv edit <remote_path>",
		"usage.open":        "用法:srv open <remote_file>",
		"usage.diff":        "用法:srv diff <local_file> [remote_file]",
		"usage.env_set":     "用法:srv env set <key> <value>",
		"usage.env_unset":   "用法:srv env unset <key>",
		"usage.env_other":   "用法:srv env [list|set|unset|clear]",

		// hint emitter
		"hint.typo_pre":  "srv: hint: %q 像是本地子命令 %q 写错了。这次还是在远端跑。",
		"hint.typo_post": "srv: hint: 远端没有 %q,看着像本地子命令 %q。试试:srv %s",

		// misc
		"misc.no_profiles_run_init": "(还没配 profile —— 先跑 `srv init`)",
		"misc.global_lang_auto":     "(auto = 走系统环境检测)",
	},
}
