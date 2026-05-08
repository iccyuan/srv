#!/usr/bin/env bash
# srv one-shot installer for macOS / Linux.
#
#   curl -fsSL https://raw.githubusercontent.com/iccyuan/srv/main/get.sh | sh
#
# Pin a version or change the install dir via env vars:
#
#   curl -fsSL .../get.sh | SRV_VERSION=2.6.5 sh
#   curl -fsSL .../get.sh | SRV_INSTALL_DIR=$HOME/.local/bin sh
#
# What it does, in order:
#   1. Detect OS (Linux / Darwin) + arch (x86_64 / arm64).
#   2. Resolve latest release version (via /releases/latest redirect)
#      unless $SRV_VERSION is set.
#   3. Download the matching .tar.gz from GitHub Releases.
#   4. Extract srv into $SRV_INSTALL_DIR (default ~/.srv/bin).
#   5. Add that dir to the right shell rc file (idempotent, marker
#      comment) unless it's already on PATH.
#   6. Print next steps -- the user runs `srv install` afterwards to
#      open the GUI for Claude Code MCP + first profile.

set -e

REPO="${SRV_REPO:-iccyuan/srv}"
INSTALL_DIR="${SRV_INSTALL_DIR:-$HOME/.srv/bin}"
VERSION="${SRV_VERSION:-}"

say() { printf '[srv get] %s\n' "$*"; }

# --- platform detection ---
case "$(uname -s)" in
    Linux)  OS=linux ;;
    Darwin) OS=macos ;;
    *)      say "unsupported OS: $(uname -s)"; exit 1 ;;
esac
case "$(uname -m)" in
    x86_64|amd64)  ARCH=x86_64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *) say "unsupported arch: $(uname -m)"; exit 1 ;;
esac

# --- resolve latest version if unspecified ---
# /releases/latest redirects to /releases/tag/vX.Y.Z; the Location
# header gives us the tag without hitting the rate-limited API.
if [ -z "$VERSION" ]; then
    say "resolving latest release..."
    VERSION=$(curl -fsSI "https://github.com/$REPO/releases/latest" \
        | tr -d '\r' \
        | awk -F/ 'tolower($1) ~ /^location:/ { print $NF }' \
        | sed 's/^v//')
fi
if [ -z "$VERSION" ]; then
    say "couldn't resolve latest version. Set SRV_VERSION=<x.y.z> manually."
    exit 1
fi

ARCHIVE="srv_${VERSION}_${OS}_${ARCH}.tar.gz"
URL="https://github.com/$REPO/releases/download/v${VERSION}/${ARCHIVE}"

say "downloading srv ${VERSION} for ${OS}/${ARCH}"
say "  url:   $URL"
say "  dest:  $INSTALL_DIR/srv"

mkdir -p "$INSTALL_DIR"
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if ! curl -fsSL "$URL" -o "$TMP/$ARCHIVE"; then
    say "download failed. Check the URL above and your network."
    exit 1
fi
tar -xzf "$TMP/$ARCHIVE" -C "$TMP"
mv "$TMP/srv" "$INSTALL_DIR/srv"
chmod +x "$INSTALL_DIR/srv"
say "installed: $("$INSTALL_DIR/srv" version)"

# --- PATH setup, only if not already on PATH ---
case ":$PATH:" in
    *":$INSTALL_DIR:"*)
        say "$INSTALL_DIR already on PATH"
        ;;
    *)
        case "$(basename "${SHELL:-}")" in
            zsh)  RC="${ZDOTDIR:-$HOME}/.zshrc" ;;
            bash)
                if [ "$(uname -s)" = "Darwin" ] && [ -f "$HOME/.bash_profile" ]; then
                    RC="$HOME/.bash_profile"
                else
                    RC="$HOME/.bashrc"
                fi
                ;;
            *) RC="$HOME/.profile" ;;
        esac
        marker="# srv installer (manage with $INSTALL_DIR/srv install)"
        if [ -f "$RC" ] && grep -qF "$marker" "$RC"; then
            say "$RC already has the srv PATH entry"
        else
            {
                printf '\n%s\n' "$marker"
                printf 'export PATH="$PATH:%s"\n' "$INSTALL_DIR"
            } >> "$RC"
            say "appended PATH export to $RC"
        fi
        ;;
esac

cat <<DONE

[srv get] done.

  Open a new shell, or:    exec \$SHELL -l
  Then to register Claude Code MCP / set up your first profile:
    srv install            (opens a browser-based wizard)

DONE
