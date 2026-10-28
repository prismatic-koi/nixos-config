# The diff.external program of the user (modules/programs/git.nix). It prints
# the builtin diff of git, so that a diff.external in a repo config cannot
# run a program.
#
# git gives these arguments: path, old file, old hash, old mode, new file,
# new hash, new mode. For a rename or a copy, it also gives the new path and
# the metadata lines. For an unmerged path, it gives only the path.

if [ "$#" -lt 7 ]; then
  printf '* Unmerged path %s\n' "$1"
  exit 0
fi

name=$1
other=${8:-$1}
# Exit 1 means "a difference" to git, so each error exits 2.
dir=$(mktemp -d) || exit 2
trap 'rm -rf -- "$dir"' EXIT

# Copies at the paths in the repo give the content diff the correct path.
old=/dev/null
new=/dev/null
if [ "$2" != /dev/null ]; then
  mkdir -p -- "$dir/a/$(dirname -- "$name")" || exit 2
  cp -- "$2" "$dir/a/$name" || exit 2
  old="a/$name"
fi
if [ "$5" != /dev/null ]; then
  mkdir -p -- "$dir/b/$(dirname -- "$other")" || exit 2
  cp -- "$5" "$dir/b/$other" || exit 2
  new="b/$other"
fi

cd -- "$dir" || exit 2
# Outside a repo, git refuses GIT_ATTR_SOURCE.
unset GIT_ATTR_SOURCE
rc=0
git diff --no-index --no-ext-diff --no-prefix -- "$old" "$new" >"$dir/out" || rc=$?
if [ "$rc" -gt 1 ]; then
  exit 2
fi

# The header and the metadata come from the arguments, because the copies
# have no rename and no file mode of the repo. They take the colour of the
# diff body.
on=""
off=""
if grep -q $'\033' "$dir/out"; then
  on=$'\033[1m'
  off=$'\033[m'
fi
meta() {
  printf '%s%s%s\n' "$on" "$1" "$off"
}
changed=0
meta "diff --git a/$name b/$other"
if [ "$2" = /dev/null ]; then
  meta "new file mode $7"
  changed=1
elif [ "$5" = /dev/null ]; then
  meta "deleted file mode $4"
  changed=1
elif [ "$4" != "$7" ]; then
  meta "old mode $4"
  meta "new mode $7"
  changed=1
fi
dropIndex=0
if [ -n "${9:-}" ]; then
  # The rename or copy lines, and the index line.
  while IFS= read -r line; do
    meta "$line"
  done <<<"${9%$'\n'}"
  dropIndex=1
  changed=1
fi

awk -v dropIndex="$dropIndex" '
  { plain = $0; gsub(/\033\[[0-9;]*m/, "", plain) }
  !body && plain ~ /^diff --git / { next }
  !body && plain ~ /^(new file mode|deleted file mode|old mode|new mode) / { next }
  !body && dropIndex && plain ~ /^index / { next }
  plain ~ /^(--- |\+\+\+ |Binary files |@@)/ { body = 1 }
  { print }
' "$dir/out" || exit 2

# diff.trustExitCode is on: 0 means no difference, 1 means a difference.
if [ "$rc" -eq 1 ]; then
  changed=1
fi
exit "$changed"
