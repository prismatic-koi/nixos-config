#!/usr/bin/env bash
# Write the theme.css.js values for one theme.
#
# Usage: theme-variables.sh <theme> [output-file]
#        theme-variables.sh --list
#   <theme>        An nx.desktop.theme value, for example everforest-light.
#   [output-file]  Default: standard output.
#   --list         Print the theme names, one for each line.
# Environment:
#   THEME_HOST     NixOS host to evaluate. Default: tui.
#   REPO_ROOT      Repository root. Default: git top-level of the current directory.
#
# The script forces nx.desktop.theme with extendModules. It changes no file
# in the repository. The nix-build step needs a Nix daemon.
set -euo pipefail

if [ "${1:-}" = "--list" ]; then
  root=${REPO_ROOT:-$(git rev-parse --show-toplevel)}
  nix-instantiate --eval --strict --json --expr \
    "(builtins.getFlake (toString $root)).nixosConfigurations.${THEME_HOST:-tui}.options.nx.desktop.theme.type.functor.payload.values" |
    tr -d '[]"' | tr ',' '\n'
  exit 0
fi

theme=${1:?usage: theme-variables.sh <theme> [output-file]}
out=${2:-/dev/stdout}
root=${REPO_ROOT:-$(git rev-parse --show-toplevel)}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

path=$(nix-build "$here/theme-variables.nix" --no-out-link \
  --argstr root "$root" --argstr theme "$theme" --argstr host "${THEME_HOST:-tui}")
cat "$path" >"$out"
