#!/usr/bin/env bash
# srv installer for macOS / Linux.
#
# Usage:
#     ./install.sh             install
#     ./install.sh --uninstall remove
#
# Strategy:
#   1. If ~/.local/bin is on PATH, symlink the binary into it (cleanest --
#      no rc-file edits, easy to remove, picks up rebuilds automatically).
#   2. Otherwise, append `export PATH="$PATH:<here>"` to the appropriate
#      shell rc file (zshrc / bashrc / bash_profile on macOS) -- guarded
#      with a marker line so re-runs don't duplicate it.

set -e

uninstall=0
gui=0
case "${1:-}" in
    --uninstall) uninstall=1 ;;
    --gui)       gui=1 ;;
esac

# Resolve the script's own directory (handles symlinks).
src="${BASH_SOURCE[0]}"
while [ -h "$src" ]; do
    dir="$(cd -P "$(dirname "$src")" && pwd)"
    src="$(readlink "$src")"
    case "$src" in
        /*) ;;
        *) src="$dir/$src" ;;
    esac
done
here="$(cd -P "$(dirname "$src")" && pwd)"
bin="$here/srv"

if [ ! -x "$bin" ]; then
    echo "srv: binary not found at $bin" >&2
    echo "Build it first:" >&2
    echo "  cd \"$here\" && go build -o srv ./cmd/srv" >&2
    exit 1
fi

# --gui hands off to the cross-platform browser-based installer baked
# into the srv binary. Same UI on Windows / macOS / Linux; covers PATH
# + Claude Code MCP + first profile in one pass.
if [ "$gui" = "1" ]; then
    exec "$bin" install
fi

# Pick the rc file for the user's shell. Falls back to ~/.profile.
shell_name="$(basename "${SHELL:-}")"
rc=""
case "$shell_name" in
    zsh)  rc="${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash)
        # macOS login shells source ~/.bash_profile; Linux uses ~/.bashrc.
        if [ "$(uname)" = "Darwin" ] && [ -f "$HOME/.bash_profile" ]; then
            rc="$HOME/.bash_profile"
        else
            rc="$HOME/.bashrc"
        fi
        ;;
    *) rc="$HOME/.profile" ;;
esac

marker="# srv installer (manage with $here/install.sh)"

# Determine whether ~/.local/bin is on the running PATH.
on_path() {
    case ":$PATH:" in
        *":$1:"*) return 0 ;;
        *) return 1 ;;
    esac
}

local_bin="$HOME/.local/bin"
target_link="$local_bin/srv"

if [ "$uninstall" = "1" ]; then
    removed=0
    if [ -L "$target_link" ]; then
        # Only remove if the link points back to our srv.
        if [ "$(readlink "$target_link")" = "$bin" ]; then
            rm "$target_link"
            echo "srv: removed symlink $target_link"
            removed=1
        fi
    fi
    if [ -f "$rc" ] && grep -qF "$marker" "$rc"; then
        # Drop the marker and the next non-empty line (the export).
        tmp="$(mktemp)"
        awk -v m="$marker" '
            $0 == m { skip = 2; next }
            skip > 0 { skip--; next }
            { print }
        ' "$rc" > "$tmp"
        mv "$tmp" "$rc"
        echo "srv: removed PATH entry from $rc"
        removed=1
    fi
    if [ "$removed" = "0" ]; then
        echo "srv: nothing to remove."
    else
        echo "Open a new shell or run:  exec \$SHELL -l"
    fi
    exit 0
fi

# Install path: prefer ~/.local/bin symlink when available.
if on_path "$local_bin"; then
    mkdir -p "$local_bin"
    ln -sfn "$bin" "$target_link"
    echo "srv: linked $target_link -> $bin"
else
    if [ -f "$rc" ] && grep -qF "$marker" "$rc"; then
        echo "srv: $rc already has the srv PATH entry"
    else
        {
            printf '\n%s\n' "$marker"
            printf 'export PATH="$PATH:%s"\n' "$here"
        } >> "$rc"
        echo "srv: appended PATH entry to $rc"
    fi
fi

# Sanity check the binary itself.
echo "srv: $("$bin" version)"
echo
echo "Done. Open a new shell, or:  exec \$SHELL -l"
echo "Then verify:                 srv version"
