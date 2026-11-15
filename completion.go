package main

import (
	"errors"
	"fmt"
	"os"
)

func cmdCompletion(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: agentbox completion bash|zsh")
	}
	var script string
	switch args[0] {
	case "bash":
		script = bashCompletion
	case "zsh":
		script = zshCompletion
	default:
		return fmt.Errorf("unsupported shell %q (expected bash or zsh)", args[0])
	}
	_, err := fmt.Fprint(os.Stdout, script)
	return err
}

func completionCandidates(args []string) error {
	if len(args) != 1 || args[0] != "sessions" {
		return errors.New("usage: agentbox __complete sessions")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(cfg.SnapshotDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && validName.MatchString(entry.Name()) {
			fmt.Println(entry.Name())
		}
	}
	return nil
}

const bashCompletion = `# Bash completion for agentbox.
_agentbox()
{
    local cur=${COMP_WORDS[COMP_CWORD]}
    local cmd=${COMP_WORDS[1]}
    local i token positional=0
    local -a commands flags

    commands=(init configure config finish daemon build start resume attach watch shell path list accept discard proxy completion)
    if (( COMP_CWORD == 1 )); then
        COMPREPLY=( $(compgen -W "${commands[*]}" -- "$cur") )
        return
    fi

    if [[ "$cmd" == completion && COMP_CWORD == 2 ]]; then
        COMPREPLY=( $(compgen -W "bash zsh" -- "$cur") )
        return
    fi

    if [[ "$cmd" == proxy ]]; then
        if (( COMP_CWORD == 2 )); then
            COMPREPLY=( $(compgen -W "up down logs" -- "$cur") )
        else
            COMPREPLY=( $(compgen -W "--project --net" -- "$cur") )
        fi
        return
    fi

    case "$cmd" in
        init) flags=(--subvolume) ;;
        configure|config) flags=(--project) ;;
        finish) flags=(--project --net) ;;
        build) flags=(--project --net --base) ;;
        start) flags=(--project --net --base --no-branch) ;;
        resume) flags=(--project --net) ;;
        shell) flags=(--project --net) ;;
        attach|watch|path|list|ls) flags=(--project --net) ;;
        accept) flags=(--project --net --promote --force --keep) ;;
        discard|rm) flags=(--project --net) ;;
        *) return ;;
    esac

    if [[ "$cur" == --net=* ]]; then COMPREPLY=( $(compgen -W "open none proxy" -- "${cur#*=}") ); return; fi
    if [[ "$cur" == --base=* ]]; then COMPREPLY=( $(compgen -W "$(git for-each-ref --format='%(refname:short)' 2>/dev/null)" -- "${cur#*=}") ); return; fi
    if [[ "$cur" == --project=* ]]; then COMPREPLY=( $(compgen -d -- "${cur#*=}") ); return; fi

    if [[ "$cur" == -* ]]; then
        COMPREPLY=( $(compgen -W "${flags[*]}" -- "$cur") )
        return
    fi

    case "${COMP_WORDS[COMP_CWORD-1]}" in
        --net) COMPREPLY=( $(compgen -W "open none proxy" -- "$cur") ); return ;;
        --base) COMPREPLY=( $(compgen -W "$(git for-each-ref --format='%(refname:short)' 2>/dev/null)" -- "$cur") ); return ;;
        --project) COMPREPLY=( $(compgen -d -- "$cur") ); return ;;
    esac

    for ((i=2; i<COMP_CWORD; i++)); do
        token=${COMP_WORDS[i]}
        case "$token" in
            --project|--net|--base) ((i++)) ;;
            --subvolume|--no-branch|--promote|--force|--keep) ;;
            -*) ;;
            *) ((positional++)) ;;
        esac
    done
    if (( positional == 0 )); then
        case "$cmd" in
            proxy) COMPREPLY=( $(compgen -W "up down logs" -- "$cur") ) ;;
            resume|attach|watch|shell|path|accept|discard|rm)
                COMPREPLY=( $(compgen -W "$(agentbox __complete sessions 2>/dev/null)" -- "$cur") )
                ;;
        esac
        return
    fi
}
complete -F _agentbox agentbox
`

const zshCompletion = `#compdef agentbox

_agentbox()
{
    local cmd=${words[2]}
    local cur=${words[CURRENT]}
    local token
    local -i i positional=0
    local -a commands flags sessions

    commands=(
        'init:write config'
        'configure:interactively configure this project'
        'config:interactively configure this project'
        'finish:remove this project config and restore a normal directory'
        'daemon:run the privileged btrfs companion daemon'
        'build:build the agent image'
        'start:create a snapshot and start an agent'
        'resume:restart an agent on its snapshot'
        'attach:reconnect to a running agent'
        'watch:follow agent output'
        'shell:open a shell in the agent container'
        'path:print the snapshot path'
        'list:list agent snapshots'
        'accept:bring agent work back'
        'discard:remove an agent snapshot'
        'proxy:manage the network proxy'
        'completion:print shell completion setup'
    )

    if (( CURRENT == 2 )); then
        _describe 'agentbox command' commands
        return
    fi
    if [[ "$cmd" == completion && CURRENT == 3 ]]; then
        compadd -- bash zsh
        return
    fi
    if [[ "$cmd" == proxy && CURRENT == 3 ]]; then
        compadd -- up down logs
        return
    fi

    case "$cmd" in
        init) flags=(--subvolume) ;;
        configure|config) flags=(--project) ;;
        finish) flags=(--project --net) ;;
        build) flags=(--project --net --base) ;;
        start) flags=(--project --net --base --no-branch) ;;
        resume) flags=(--project --net) ;;
        shell) flags=(--project --net) ;;
        attach|watch|path|list|ls) flags=(--project --net) ;;
        accept) flags=(--project --net --promote --force --keep) ;;
        discard|rm) flags=(--project --net) ;;
        proxy) flags=(--project --net) ;;
        *) return ;;
    esac

    if [[ "$cur" == --net ]]; then
        compadd -- open none proxy
        return
    elif [[ "$cur" == --project ]]; then
        _files -/
        return
    elif [[ "${words[CURRENT-1]}" == --net ]]; then
        compadd -- open none proxy
        return
    elif [[ "${words[CURRENT-1]}" == --project ]]; then
        _files -/
        return
    elif [[ "${words[CURRENT-1]}" == --base ]]; then
        _alternative 'refs:git refs:($(git for-each-ref --format="%(refname:short)" 2>/dev/null))'
        return
    elif [[ "$cur" == --net=* ]]; then
        compadd -- open none proxy
        return
    elif [[ "$cur" == --base=* ]]; then
        _alternative 'refs:git refs:($(git for-each-ref --format="%(refname:short)" 2>/dev/null))'
        return
    elif [[ "$cur" == --project=* ]]; then
        _files -/
        return
    elif [[ "$cur" == -* ]]; then
        compadd -- $flags
        return
    fi

    for ((i=3; i<CURRENT; i++)); do
        token=${words[i]}
        case "$token" in
            --project|--net|--base) ((i++)) ;;
            --subvolume|--no-branch|--promote|--force|--keep) ;;
            -*) ;;
            *) ((positional++)) ;;
        esac
    done
    if (( positional == 0 )); then
        case "$cmd" in
            resume|attach|watch|shell|path|accept|discard|rm)
                sessions=(${(f)"$(agentbox __complete sessions 2>/dev/null)"})
                compadd -- $sessions
                ;;
            proxy) compadd -- up down logs ;;
        esac
    fi
}

compdef _agentbox agentbox
`
