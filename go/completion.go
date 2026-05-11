package main

import (
	"fmt"
	"os"
	"strings"
)

const bashCompletion = `# srv bash completion
_srv() {
    local cur prev
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]:-}"
    local subs="__SRV_SUBS__"

    # Track first and second positional args, skipping global flags. The
    # AST-style tokens give us context for nested completion (e.g. for
    # 'config use <prof>' we need both 'config' and 'use').
    local sub="" sub2=""
    local i=1
    while [[ $i -lt $COMP_CWORD ]]; do
        case "${COMP_WORDS[i]}" in
            -P|--profile) i=$((i+1)) ;;
            --profile=*|-t|--tty|-d|--detach) ;;
            *)
                if [[ -z $sub ]]; then sub="${COMP_WORDS[i]}"
                elif [[ -z $sub2 ]]; then sub2="${COMP_WORDS[i]}"
                fi
                ;;
        esac
        i=$((i+1))
    done

    # Profile-name completion right after -P / --profile.
    if [[ "$prev" == "-P" || "$prev" == "--profile" ]]; then
        local profs
        profs=$(srv _profiles 2>/dev/null)
        COMPREPLY=( $(compgen -W "$profs" -- "$cur") )
        return 0
    fi

    # First positional: complete from the subcommand list. We must
    # return 0 here -- "return nil" (the previous typo) is not a valid
    # exit code in bash and triggered "numeric argument required" on
    # every TAB. Under bash-completion or any -o bashdefault setup that
    # non-zero exit fell through to local file completion, so
    # "srv con" + TAB looked like it was completing filenames starting
    # with "con" instead of suggesting config / completion.
    if [[ -z "$sub" ]]; then
        COMPREPLY=( $(compgen -W "$subs" -- "$cur") )
        return 0
    fi

    # Helper: load remote entries from 'srv _ls' into COMPREPLY (preserves
    # spaces in names). Optional first arg "dirs" filters dirs-only.
    # MSYS_NO_PATHCONV=1 stops git-bash from mangling absolute paths like
    # /opt/ into C:/Program Files/Git/opt/ when invoking the native exe.
    _srv_remote_ls() {
        local mode="${1:-all}" line
        local -a out=()
        while IFS= read -r line; do
            [[ -z $line ]] && continue
            if [[ $mode == "dirs" && $line != */ ]]; then continue; fi
            out+=("$line")
        done < <(MSYS_NO_PATHCONV=1 srv _ls "$cur" 2>/dev/null)
        COMPREPLY=("${out[@]}")
        # Don't auto-append a space, so user can keep typing path components.
        compopt -o nospace 2>/dev/null
    }

    case "$sub" in
__SRV_CASES__
        *)
            # Catch-all: srv treats unrecognized first tokens as remote
            # commands. Args of those (paths, etc.) should complete from
            # remote, not from the local cwd. Without this, bash-completion
            # users would see local filenames leak in via -o bashdefault.
            _srv_remote_ls all
            ;;
    esac
}
complete -F _srv srv
`

const zshCompletion = `#compdef srv
_srv() {
    local -a subs
    subs=(
        'init:configure a profile'
        'config:manage profiles'
        'use:pin a profile for this shell'
        'cd:change persistent remote cwd'
        'pwd:show remote cwd'
        'status:show profile and cwd'
        'check:probe SSH connectivity and diagnose failures'
        'shell:interactive remote shell'
        'run:run a command on remote'
        'push:upload via SFTP'
        'pull:download via SFTP'
        'sync:bulk-sync changed files (git/mtime/glob/list)'
        'tunnel:forward a local port to a remote port'
        'edit:open a remote file in $EDITOR, save back on close'
        'open:pull a remote file to a temp dir and open it locally'
        'code:open a remote folder in VS Code Remote SSH'
        'diff:compare a local file with the remote counterpart'
        'doctor:local config / daemon / SSH readiness report'
        'env:manage profile-level remote environment variables'
        'jobs:list detached jobs'
        'logs:tail a detached job log'
        'kill:terminate a detached job'
        'sessions:list/manage shell sessions'
        'completion:emit shell completion script'
        'mcp:run as a stdio MCP server'
        'help:show help'
        'version:show version'
    )

    # Track first and second positional args, skipping global flags so
    # 'srv -P prod cd <TAB>' still routes to remote-dir completion.
    local sub sub2 i=2 token
    while (( i < CURRENT )); do
        token=$words[i]
        case $token in
            -P|--profile) (( i++ )) ;;
            --profile=*|-t|--tty|-d|--detach) ;;
            *)
                if [[ -z $sub ]]; then sub=$token
                elif [[ -z $sub2 ]]; then sub2=$token
                fi
                ;;
        esac
        (( i++ ))
    done

    # Profile-name completion right after -P / --profile.
    if [[ $words[CURRENT-1] == (-P|--profile) ]]; then
        local profs
        profs=("${(@f)$(srv _profiles 2>/dev/null)}")
        _values 'profile' $profs
        return
    fi

    if [[ -z $sub ]]; then
        _describe 'subcommand' subs
        return
    fi

    # Remote ls helper. Pass "dirs" to filter to directories only.
    _srv_remote_ls() {
        local mode="${1:-all}" entries
        entries=("${(@f)$(srv _ls $words[CURRENT] 2>/dev/null)}")
        if [[ $mode == "dirs" ]]; then
            entries=(${(M)entries:#*/})
        fi
        compadd -S '' -- $entries
    }

    case "$sub" in
__SRV_CASES__
        # Catch-all: srv runs unrecognized tokens on the remote, so their
        # args should complete remotely too. Without this, zsh would fall
        # back to _default (local files) and leak local cwd entries.
        *) _srv_remote_ls all ;;
    esac
}
_srv "$@"
`

const powershellCompletion = `# srv PowerShell completion
# Registered for both 'srv' and 'srv.exe' so users who invoke either
# form (` + "`" + `srv con<TAB>` + "`" + ` vs ` + "`" + `srv.exe con<TAB>` + "`" + `) get the same suggestions.
Register-ArgumentCompleter -Native -CommandName srv,srv.exe -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)

    $mk = { param($t) [System.Management.Automation.CompletionResult]::new($t, $t, 'ParameterValue', $t) }
    $emit = { param($items) $items | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object { & $mk $_ } }
    $profiles = { @((& srv _profiles 2>$null) -split "` + "`" + `n" | Where-Object { $_ }) }

    # Strip the command element and find positional tokens after it.
    # @(...) is required: a single-item pipeline collapses to a scalar in PS,
    # which would make foreach iterate characters of the string.
    # The basename check (Split-Path -Leaf) covers cases where the AST
    # surfaced the full path (e.g. 'C:\path\srv.exe') instead of the bare
    # command name.
    $tokens = @($commandAst.CommandElements |
        ForEach-Object { $_.ToString() } |
        Where-Object {
            $leaf = try { [System.IO.Path]::GetFileName($_) } catch { $_ }
            $leaf -ne 'srv' -and $leaf -ne 'srv.exe'
        })
    $positional = @()
    $skip = $false
    foreach ($t in $tokens) {
        if ($skip) { $skip = $false; continue }
        if ($t -in '-P', '--profile') { $skip = $true; continue }
        if ($t -like '--profile=*' -or $t -in '-t', '--tty', '-d', '--detach') { continue }
        # Skip the partial token at the cursor (the one we're completing).
        if ($t -eq $wordToComplete) { continue }
        $positional += $t
    }
    $sub  = if ($positional.Count -ge 1) { $positional[0] } else { $null }
    $sub2 = if ($positional.Count -ge 2) { $positional[1] } else { $null }

    # Profile-name completion after -P / --profile.
    $prevToken = if ($tokens.Count -gt 0 -and $tokens[-1] -ne $wordToComplete) {
        $tokens[-1]
    } elseif ($tokens.Count -gt 1) {
        $tokens[-2]
    } else { $null }
    if ($prevToken -in '-P', '--profile') {
        & $emit (& $profiles)
        return
    }

    $subs = @(__SRV_SUBS_PS__)
    if (-not $sub) {
        & $emit $subs
        return
    }

    # Local file/dir completer for push <local>, pull <local>, diff <local>.
    # Uses PowerShell's built-in CompleteFilename so drive letters (C:),
    # tilde paths (~/foo), UNC roots (\\server\share), and quoted paths
    # all work the same as they do for any other PS cmdlet. The previous
    # Get-ChildItem -Path "$wordToComplete*" approach matched literally,
    # so "srv push C:<TAB>" just searched cwd for files starting with
    # "C:" (always nothing) instead of listing the C drive root.
    $local_files = {
        [System.Management.Automation.CompletionCompleters]::CompleteFilename($wordToComplete)
    }

    # Remote ls helper: invokes 'srv _ls <prefix>' and emits each matching
    # full path (dirs get a trailing /). Used for cd / pull / push-remote /
    # run / exec / catch-all.
    #
    # When 0 remote entries match we MUST still emit something, otherwise
    # PowerShell 5.1's native completer falls back to local file completion
    # (ProviderItem results from cwd) and leaks local content into srv
    # commands. Emitting the input word unchanged is the standard "claim
    # this slot, no-op the keypress" trick: TAB visibly does nothing, but
    # it doesn't drag in local files either.
    $remote_ls = {
        param($onlyDirs)
        $rs = & srv _ls $wordToComplete 2>$null
        $emitted = 0
        foreach ($line in $rs -split "` + "`" + `n") {
            if (-not $line) { continue }
            if ($onlyDirs -and -not $line.EndsWith('/')) { continue }
            & $mk $line
            $emitted++
        }
        if ($emitted -eq 0) {
            # Non-empty CompletionText is required: PS 5.1 silently drops
            # results whose CompletionText is empty, then falls back to the
            # FileSystem provider. We feed the typed word back when present
            # (visibly a no-op), else a single space (also a near-no-op).
            $stub = if ($wordToComplete) { $wordToComplete } else { ' ' }
            [System.Management.Automation.CompletionResult]::new(
                $stub, '(no remote matches)', 'ParameterValue', '(no remote matches)')
        }
    }

    switch ($sub) {
__SRV_CASES__
        default {
            # Catch-all: srv routes unrecognized first tokens to the remote.
            # Their args should complete remotely too. Without this branch
            # PowerShell's native completer falls back to local file
            # completion when the script block returns 0 results, leaking
            # local cwd contents into srv completions.
            & $remote_ls $false
        }
    }
}
`

func cmdCompletion(args []string) error {
	if len(args) == 0 {
		return exitErr(1, "usage: srv completion <bash|zsh|powershell>")
	}
	// The bash and PowerShell scripts get their `subs` list rendered
	// from the live registry so adding a new subcommand only requires
	// a commands.go entry, not three parallel hand-edits. zsh keeps its
	// hand-curated array because each entry carries a description shown
	// in the `_describe` menu -- worth preserving over OCP purity.
	bashSubs := strings.Join(userVisibleSubcommands(), " ")
	psSubs := "'" + strings.Join(userVisibleSubcommands(), "','") + "'"

	switch args[0] {
	case "bash":
		// bash users have srv on PATH (otherwise they couldn't run
		// `srv completion bash` in the first place); leave the inline
		// `srv _profiles` to use PATH lookup.
		out := strings.ReplaceAll(bashCompletion, "__SRV_SUBS__", bashSubs)
		out = strings.ReplaceAll(out, "__SRV_CASES__", emitBashCases())
		fmt.Print(out)
	case "zsh":
		out := strings.ReplaceAll(zshCompletion, "__SRV_CASES__", emitZshCases())
		fmt.Print(out)
	case "powershell", "pwsh", "ps":
		// PowerShell argument-completer scopes don't always inherit the
		// expected PATH for native command lookup. Burn the absolute path
		// of this binary into the script so `_profiles` always resolves.
		self, err := os.Executable()
		if err != nil || self == "" {
			self = "srv"
		}
		quoted := "'" + strings.ReplaceAll(self, "'", "''") + "'"
		out := powershellCompletion
		out = strings.ReplaceAll(out, "__SRV_SUBS_PS__", psSubs)
		out = strings.ReplaceAll(out, "__SRV_CASES__", emitPSCases())
		out = strings.ReplaceAll(out, "& srv _profiles", "& "+quoted+" _profiles")
		out = strings.ReplaceAll(out, "& srv _ls", "& "+quoted+" _ls")
		fmt.Print(out)
	default:
		return exitErr(1, "error: unknown shell %q (expected bash/zsh/powershell)", args[0])
	}
	return nil
}
