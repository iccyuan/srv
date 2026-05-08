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
    local subs="init config use cd pwd status check doctor shell run exec push pull sync tunnel edit open code diff env jobs logs kill sessions completion mcp daemon help version"

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
        config)
            if [[ -z $sub2 ]]; then
                COMPREPLY=( $(compgen -W "list default global remove show set edit" -- "$cur") )
            elif [[ "$sub2" == "default" || "$sub2" == "remove" || "$sub2" == "show" || "$sub2" == "edit" ]]; then
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
        cd)
            _srv_remote_ls dirs
            ;;
        edit)
            _srv_remote_ls all
            ;;
        pull)
            if [[ -z $sub2 ]]; then _srv_remote_ls all
            else COMPREPLY=( $(compgen -f -- "$cur") )
            fi
            ;;
        push)
            if [[ -z $sub2 ]]; then COMPREPLY=( $(compgen -f -- "$cur") )
            else _srv_remote_ls all
            fi
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
        config)
            if [[ -z $sub2 ]]; then
                _values 'action' list default global remove show set edit
            elif [[ $sub2 == (default|remove|show|edit) ]]; then
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
        cd) _srv_remote_ls dirs ;;
        edit) _srv_remote_ls all ;;
        pull)
            if [[ -z $sub2 ]]; then _srv_remote_ls all
            else _files
            fi
            ;;
        push)
            if [[ -z $sub2 ]]; then _files
            else _srv_remote_ls all
            fi
            ;;
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

    $subs = 'init','config','use','cd','pwd','status','check','doctor','shell','run','exec','push','pull','sync','tunnel','edit','open','code','diff','env','jobs','logs','kill','sessions','completion','mcp','daemon','help','version'
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
                & $emit @('list','default','global','remove','show','set','edit')
            } elseif ($sub2 -in 'default','remove','show','edit') {
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
        'edit' {
            & $remote_ls $false
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
