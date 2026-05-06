package main

import (
	"fmt"
	"os"
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
    $tokens = $commandAst.CommandElements |
        ForEach-Object { $_.ToString() } |
        Where-Object { $_ -ne 'srv' -and $_ -ne 'srv.exe' }
    $skip = $false
    $sub = $null
    foreach ($t in $tokens) {
        if ($skip) { $skip = $false; continue }
        if ($t -in '-P', '--profile') { $skip = $true; continue }
        if ($t -like '--profile=*' -or $t -in '-t', '--tty', '-d', '--detach') { continue }
        $sub = $t
        break
    }
    $subs = 'init','config','use','cd','pwd','status','check','run','exec','push','pull','sync','jobs','logs','kill','sessions','completion','mcp','help','version'
    if (-not $sub -or $sub -eq $wordToComplete) {
        $subs | Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        return
    }
    switch ($sub) {
        'config' {
            'list','use','remove','show','set' |
                Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'use' {
            $profs = (& srv _profiles 2>$null) -split "` + "`" + `n" | Where-Object { $_ }
            ($profs + '--clear') | Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'sessions' {
            'list','show','clear','prune' |
                Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'completion' {
            'bash','zsh','powershell' |
                Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
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
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	case "powershell", "pwsh", "ps":
		fmt.Print(powershellCompletion)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown shell %q (expected bash/zsh/powershell)\n", args[0])
		return 1
	}
	return 0
}
