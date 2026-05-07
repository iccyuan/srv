package main

import (
	"fmt"
	"os"
	"strings"
)

const bashCompletion = `# srv bash completion
_srv() {
    local cur prev words cword
    COMPREPLY=()
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]:-}"
    local subs="init config use cd pwd status check run exec push pull sync jobs logs kill sessions completion mcp help version"
    local sub=""
    local i
    for ((i=1; i<COMP_CWORD; i++)); do
        case "${COMP_WORDS[i]}" in
            -P|--profile) i=$((i+1)) ;;
            --profile=*|-t|--tty|-d|--detach) ;;
            *) sub="${COMP_WORDS[i]}"; break ;;
        esac
    done
    if [[ -z "$sub" ]]; then
        COMPREPLY=( $(compgen -W "$subs" -- "$cur") )
        return 0
    fi
    case "$sub" in
        config)
            local action=""
            for ((i=1; i<COMP_CWORD; i++)); do
                case "${COMP_WORDS[i]}" in
                    -P|--profile) i=$((i+1)) ;;
                    config) ;;
                    *) action="${COMP_WORDS[i]}"; break ;;
                esac
            done
            if [[ "$action" == "config" || -z "$action" ]]; then
                COMPREPLY=( $(compgen -W "list use remove show set" -- "$cur") )
            elif [[ "$action" == "use" || "$action" == "remove" || "$action" == "show" ]]; then
                local profs
                profs=$(srv _profiles 2>/dev/null)
                COMPREPLY=( $(compgen -W "$profs" -- "$cur") )
            fi
            ;;
        use)
            local profs
            profs=$(srv _profiles 2>/dev/null)
            COMPREPLY=( $(compgen -W "$profs --clear" -- "$cur") )
            ;;
        sessions)
            COMPREPLY=( $(compgen -W "list show clear prune" -- "$cur") )
            ;;
        completion)
            COMPREPLY=( $(compgen -W "bash zsh powershell" -- "$cur") )
            ;;
        push)
            COMPREPLY=( $(compgen -f -- "$cur") )
            ;;
    esac
    if [[ "$prev" == "-P" || "$prev" == "--profile" ]]; then
        local profs
        profs=$(srv _profiles 2>/dev/null)
        COMPREPLY=( $(compgen -W "$profs" -- "$cur") )
    fi
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
        'run:run a command on remote'
        'push:upload via SFTP'
        'pull:download via SFTP'
        'sync:bulk-sync changed files (git/mtime/glob/list)'
        'jobs:list detached jobs'
        'logs:tail a detached job log'
        'kill:terminate a detached job'
        'sessions:list/manage shell sessions'
        'completion:emit shell completion script'
        'mcp:run as a stdio MCP server'
        'help:show help'
        'version:show version'
    )
    if (( CURRENT == 2 )); then
        _describe 'subcommand' subs
        return
    fi
    case "$words[2]" in
        config)
            if (( CURRENT == 3 )); then
                _values 'action' list use remove show set
            elif [[ "$words[3]" == (use|remove|show) ]]; then
                local profs
                profs=("${(@f)$(srv _profiles 2>/dev/null)}")
                _values 'profile' $profs
            fi
            ;;
        use)
            local profs
            profs=("${(@f)$(srv _profiles 2>/dev/null)}")
            _values 'profile' $profs --clear
            ;;
        sessions) _values 'action' list show clear prune ;;
        completion) _values 'shell' bash zsh powershell ;;
        push|pull) _files ;;
    esac
}
_srv "$@"
`

const powershellCompletion = `# srv PowerShell completion
Register-ArgumentCompleter -Native -CommandName srv -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)

    $mk = { param($t) [System.Management.Automation.CompletionResult]::new($t, $t, 'ParameterValue', $t) }
    $emit = { param($items) $items | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object { & $mk $_ } }
    $profiles = { @((& srv _profiles 2>$null) -split "` + "`" + `n" | Where-Object { $_ }) }

    # Strip global flags and find positional tokens after them.
    # @(...) is required: a single-item pipeline collapses to a scalar in PS,
    # which would make foreach iterate characters of the string.
    $tokens = @($commandAst.CommandElements |
        ForEach-Object { $_.ToString() } |
        Where-Object { $_ -ne 'srv' -and $_ -ne 'srv.exe' })
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

    $subs = 'init','config','use','cd','pwd','status','check','run','exec','push','pull','sync','jobs','logs','kill','sessions','completion','mcp','help','version'
    if (-not $sub) {
        & $emit $subs
        return
    }

    # Remote ls helper: invokes 'srv _ls <prefix>' and emits each matching
    # full path (dirs get a trailing /). Used for cd / pull / push-remote.
    $remote_ls = {
        param($onlyDirs)
        $rs = & srv _ls $wordToComplete 2>$null
        foreach ($line in $rs -split "` + "`" + `n") {
            if (-not $line) { continue }
            if ($onlyDirs -and -not $line.EndsWith('/')) { continue }
            & $mk $line
        }
    }

    switch ($sub) {
        'config' {
            if (-not $sub2) {
                & $emit @('list','use','remove','show','set')
            } elseif ($sub2 -in 'use','remove','show') {
                & $emit (& $profiles)
            }
        }
        'use' {
            & $emit (@(& $profiles) + '--clear')
        }
        'sessions' {
            & $emit @('list','show','clear','prune')
        }
        'completion' {
            & $emit @('bash','zsh','powershell')
        }
        'cd' {
            # Remote directory completion (only dirs).
            & $remote_ls $true
        }
        'pull' {
            # First positional = remote path (any entry).
            if (-not $sub2) {
                & $remote_ls $false
            } else {
                # Second positional = local file.
                Get-ChildItem -Path "$wordToComplete*" -ErrorAction SilentlyContinue |
                    ForEach-Object { & $mk $_.Name }
            }
        }
        'push' {
            if (-not $sub2) {
                # First positional = local file/dir.
                Get-ChildItem -Path "$wordToComplete*" -ErrorAction SilentlyContinue |
                    ForEach-Object { & $mk $_.Name }
            } else {
                # Second positional = remote path (any entry).
                & $remote_ls $false
            }
        }
    }
}
`

func cmdCompletion(args []string) int {
	if len(args) == 0 {
		fatal("usage: srv completion <bash|zsh|powershell>")
	}
	switch args[0] {
	case "bash":
		// bash users have srv on PATH (otherwise they couldn't run
		// `srv completion bash` in the first place); leave the inline
		// `srv _profiles` to use PATH lookup.
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
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
		out = strings.ReplaceAll(out, "& srv _profiles", "& "+quoted+" _profiles")
		out = strings.ReplaceAll(out, "& srv _ls", "& "+quoted+" _ls")
		fmt.Print(out)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown shell %q (expected bash/zsh/powershell)\n", args[0])
		return 1
	}
	return 0
}
