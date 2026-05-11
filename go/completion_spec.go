package main

import (
	"fmt"
	"sort"
	"strings"
)

// Completion DSL. compSpec describes a subcommand's positional argument
// shapes once; per-shell generators (emitBashCases / emitZshCases /
// emitPSCases below) translate that into the bash, zsh, and PowerShell
// case-block syntax they each need.
//
// One source of truth means changing what `srv push <local> <remote>`
// completes is a single-line edit in compSpecs, not three parallel
// edits across hand-written shell snippets that historically drifted
// apart whenever someone forgot a shell.

type argType int

const (
	argNone        argType = iota // no completion
	argRemotePath                 // any remote entry (file or dir)
	argRemoteDir                  // remote dirs only (cd)
	argLocalFile                  // local file/dir
	argProfile                    // a configured profile name
	argEnum                       // a fixed enum (subactions, shell names...)
	argEnumProfile                // a fixed enum *plus* the profile list (use)
)

type argSlot struct {
	typ     argType
	choices []string // populated when typ == argEnum / argEnumProfile
}

type compSpec struct {
	// positions[i] = type of the (i+1)th token after the subcommand
	// name. positions[0] is the very first arg.
	positions []argSlot
	// rest applies to every arg beyond len(positions). argNone = no
	// further completion.
	rest argType
	// actions: when positions[0] is argEnum and the user already picked
	// an action, the next positional follows the per-action sub-spec
	// instead of falling off the end. Drives `config <action> <args>`.
	actions map[string]*compSpec
}

// compSpecs is the registry. Subcommands NOT listed here fall through
// to the default catch-all (remote completion) which is the right
// behaviour for `srv <unknown> <args>` (treated as a remote command).
//
// Some commands deliberately have no entry: status / pwd / init /
// version / help take no args, so any user input there is "garbage"
// from completion's perspective -- the catch-all returns nothing
// useful but also doesn't leak local files (PS no-op stub takes care
// of that).
var compSpecs = map[string]compSpec{
	"cd":   {positions: []argSlot{{typ: argRemoteDir}}},
	"edit": {positions: []argSlot{{typ: argRemotePath}}},
	"open": {positions: []argSlot{{typ: argRemotePath}}},
	"code": {positions: []argSlot{{typ: argRemoteDir}}},
	"pull": {positions: []argSlot{{typ: argRemotePath}, {typ: argLocalFile}}},
	"push": {positions: []argSlot{{typ: argLocalFile}, {typ: argRemotePath}}},
	"diff": {positions: []argSlot{{typ: argLocalFile}, {typ: argRemotePath}}},
	"run":  {rest: argRemotePath},
	"exec": {rest: argRemotePath},

	"use": {positions: []argSlot{{typ: argEnumProfile, choices: []string{"--clear"}}}},

	"sessions":   {positions: []argSlot{{typ: argEnum, choices: []string{"list", "show", "clear", "prune"}}}},
	"completion": {positions: []argSlot{{typ: argEnum, choices: []string{"bash", "zsh", "powershell"}}}},
	"group":      {positions: []argSlot{{typ: argEnum, choices: []string{"list", "show", "set", "remove"}}}},
	"guard":      {positions: []argSlot{{typ: argEnum, choices: []string{"on", "off", "status"}}}},
	"daemon":     {positions: []argSlot{{typ: argEnum, choices: []string{"status", "stop", "restart", "logs", "prune-cache"}}}},
	"color":      {positions: []argSlot{{typ: argEnum, choices: []string{"on", "off", "auto", "use", "list", "status"}}}},
	"env":        {positions: []argSlot{{typ: argEnum, choices: []string{"list", "set", "unset", "clear"}}}},

	"config": {
		positions: []argSlot{{typ: argEnum, choices: []string{"list", "default", "global", "remove", "show", "set", "edit"}}},
		actions: map[string]*compSpec{
			"default": {positions: []argSlot{{typ: argProfile}}},
			"remove":  {positions: []argSlot{{typ: argProfile}}},
			"show":    {positions: []argSlot{{typ: argProfile}}},
			"edit":    {positions: []argSlot{{typ: argProfile}}},
		},
	},
}

// --- bash generator ----------------------------------------------------

func emitBashCases() string {
	var b strings.Builder
	names := sortedSpecNames()
	for _, name := range names {
		spec := compSpecs[name]
		b.WriteString("        " + name + ")\n")
		b.WriteString(emitBashBody(spec, "            "))
		b.WriteString("            ;;\n")
	}
	return b.String()
}

func emitBashBody(spec compSpec, indent string) string {
	// Single-position spec: one if-else over $sub2.
	if len(spec.positions) == 0 {
		return indent + bashEmit(spec.rest) + "\n"
	}
	if len(spec.positions) == 1 && len(spec.actions) == 0 {
		return indent + bashEmit2(spec.positions[0]) + "\n"
	}
	// Two positions or actions: switch on $sub2.
	var sb strings.Builder
	if len(spec.actions) > 0 {
		sb.WriteString(indent + "if [[ -z $sub2 ]]; then\n")
		sb.WriteString(indent + "    " + bashEmit2(spec.positions[0]) + "\n")
		sb.WriteString(indent + "else\n")
		sb.WriteString(indent + "    case \"$sub2\" in\n")
		actions := sortedActionNames(spec.actions)
		for _, a := range actions {
			act := spec.actions[a]
			if len(act.positions) == 0 {
				continue
			}
			sb.WriteString(indent + "        " + a + ") " + bashEmit2(act.positions[0]) + " ;;\n")
		}
		sb.WriteString(indent + "    esac\n")
		sb.WriteString(indent + "fi\n")
		return sb.String()
	}
	// Two-position positional (push/pull/diff).
	sb.WriteString(indent + "if [[ -z $sub2 ]]; then " + bashEmit2(spec.positions[0]) + "\n")
	sb.WriteString(indent + "else " + bashEmit2(spec.positions[1]) + "\n")
	sb.WriteString(indent + "fi\n")
	return sb.String()
}

// bashEmit2 emits a single bash completion call for one slot, ending
// without a trailing newline -- the caller decides whether to wrap in
// `if/else` or terminate.
func bashEmit2(s argSlot) string {
	return bashEmitSlot(s)
}

func bashEmit(t argType) string {
	return bashEmitSlot(argSlot{typ: t})
}

func bashEmitSlot(s argSlot) string {
	switch s.typ {
	case argRemotePath:
		return "_srv_remote_ls all"
	case argRemoteDir:
		return "_srv_remote_ls dirs"
	case argLocalFile:
		return `COMPREPLY=( $(compgen -f -- "$cur") )`
	case argProfile:
		return `local profs; profs=$(srv _profiles 2>/dev/null); COMPREPLY=( $(compgen -W "$profs" -- "$cur") )`
	case argEnum:
		return fmt.Sprintf(`COMPREPLY=( $(compgen -W "%s" -- "$cur") )`, strings.Join(s.choices, " "))
	case argEnumProfile:
		extra := strings.Join(s.choices, " ")
		return fmt.Sprintf(`local profs; profs=$(srv _profiles 2>/dev/null); COMPREPLY=( $(compgen -W "$profs %s" -- "$cur") )`, extra)
	}
	return ""
}

// --- zsh generator -----------------------------------------------------

func emitZshCases() string {
	var b strings.Builder
	names := sortedSpecNames()
	for _, name := range names {
		spec := compSpecs[name]
		b.WriteString("        " + name + ") " + emitZshBody(spec) + " ;;\n")
	}
	return b.String()
}

func emitZshBody(spec compSpec) string {
	if len(spec.positions) == 0 {
		return zshEmitSlot(argSlot{typ: spec.rest})
	}
	if len(spec.positions) == 1 && len(spec.actions) == 0 {
		return zshEmitSlot(spec.positions[0])
	}
	if len(spec.actions) > 0 {
		var sb strings.Builder
		sb.WriteString("if [[ -z $sub2 ]]; then " + zshEmitSlot(spec.positions[0]))
		sb.WriteString("; else case $sub2 in ")
		actions := sortedActionNames(spec.actions)
		for _, a := range actions {
			act := spec.actions[a]
			if len(act.positions) == 0 {
				continue
			}
			sb.WriteString(a + ") " + zshEmitSlot(act.positions[0]) + ";; ")
		}
		sb.WriteString("esac; fi")
		return sb.String()
	}
	return "if [[ -z $sub2 ]]; then " + zshEmitSlot(spec.positions[0]) +
		"; else " + zshEmitSlot(spec.positions[1]) + "; fi"
}

func zshEmitSlot(s argSlot) string {
	switch s.typ {
	case argRemotePath:
		return "_srv_remote_ls all"
	case argRemoteDir:
		return "_srv_remote_ls dirs"
	case argLocalFile:
		return "_files"
	case argProfile:
		return `local profs; profs=("${(@f)$(srv _profiles 2>/dev/null)}"); _values 'profile' $profs`
	case argEnum:
		return "_values 'action' " + strings.Join(s.choices, " ")
	case argEnumProfile:
		extra := strings.Join(s.choices, " ")
		return fmt.Sprintf(`local profs; profs=("${(@f)$(srv _profiles 2>/dev/null)}"); _values 'profile' $profs %s`, extra)
	}
	return ""
}

// --- PowerShell generator ---------------------------------------------

func emitPSCases() string {
	var b strings.Builder
	names := sortedSpecNames()
	for _, name := range names {
		spec := compSpecs[name]
		b.WriteString("        '" + name + "' { " + emitPSBody(spec) + " }\n")
	}
	return b.String()
}

func emitPSBody(spec compSpec) string {
	if len(spec.positions) == 0 {
		return psEmitSlot(argSlot{typ: spec.rest})
	}
	if len(spec.positions) == 1 && len(spec.actions) == 0 {
		return psEmitSlot(spec.positions[0])
	}
	if len(spec.actions) > 0 {
		var sb strings.Builder
		sb.WriteString("if (-not $sub2) { " + psEmitSlot(spec.positions[0]) + " } else { switch ($sub2) { ")
		actions := sortedActionNames(spec.actions)
		for _, a := range actions {
			act := spec.actions[a]
			if len(act.positions) == 0 {
				continue
			}
			sb.WriteString(fmt.Sprintf("'%s' { %s }; ", a, psEmitSlot(act.positions[0])))
		}
		sb.WriteString("} }")
		return sb.String()
	}
	return "if (-not $sub2) { " + psEmitSlot(spec.positions[0]) +
		" } else { " + psEmitSlot(spec.positions[1]) + " }"
}

func psEmitSlot(s argSlot) string {
	switch s.typ {
	case argRemotePath:
		return "& $remote_ls $false"
	case argRemoteDir:
		return "& $remote_ls $true"
	case argLocalFile:
		return "& $local_files"
	case argProfile:
		return "& $emit (& $profiles)"
	case argEnum:
		quoted := make([]string, len(s.choices))
		for i, c := range s.choices {
			quoted[i] = "'" + c + "'"
		}
		return "& $emit @(" + strings.Join(quoted, ",") + ")"
	case argEnumProfile:
		quoted := make([]string, len(s.choices))
		for i, c := range s.choices {
			quoted[i] = "'" + c + "'"
		}
		return "& $emit (@(& $profiles) + " + strings.Join(quoted, ",") + ")"
	}
	return ""
}

// --- helpers ----------------------------------------------------------

func sortedSpecNames() []string {
	names := make([]string, 0, len(compSpecs))
	for n := range compSpecs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func sortedActionNames(m map[string]*compSpec) []string {
	out := make([]string, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
