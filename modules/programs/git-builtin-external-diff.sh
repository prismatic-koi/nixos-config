# The diff.external program of the user (modules/programs/git.nix). It prints
# the builtin diff of git, so that a diff.external in a repo config cannot run
# a program. git has no config value that means "use the builtin diff", so the
# user pins the key to this program.
#
# git gives these arguments: path, old file, old hash, old mode, new file,
# new hash, new mode. For a rename or a copy, it also gives the new path ($8).
# For an unmerged path, it gives only the path.
#
# This program runs no command and no code that the repo controls:
#   - It unsets each variable that names a git dir, so the inner git reads no
#     repo config and no .gitattributes. It also unsets GIT_ATTR_SOURCE, which
#     makes git abort outside a repo.
#   - It runs git only with --no-index on two files in a temp dir.
#   - It writes only in the temp dir. It refuses a path that is absolute or
#     that has a ".." part, so a crafted path (from a git tree or an index)
#     cannot escape the temp dir.
# An error exits 2, because diff.trustExitCode reads exit 1 as "a difference".
#
# The output is an approximation of the builtin diff. It is the same for the
# common case (a content change). It does not reproduce the command-line diff
# options (for example -w, -U<n>, --word-diff, --binary), the file mode on the
# header, a rename, or a symlink type. For exact output, use
# git diff --no-ext-diff.

if [ "$#" -lt 7 ]; then
  printf '* Unmerged path %s\n' "$1"
  exit 0
fi

name=$1
other=${8:-$1}

# Refuse a path that can escape the temp dir.
for p in "$name" "$other"; do
  case "$p" in
    /*) exit 2 ;;
  esac
  case "/$p/" in
    */../*) exit 2 ;;
  esac
done

# git must not find a repo or an attributes file from the temp dir.
unset GIT_DIR GIT_COMMON_DIR GIT_WORK_TREE GIT_INDEX_FILE \
  GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE \
  GIT_PREFIX GIT_ATTR_SOURCE GIT_CONFIG_COUNT

dir=$(mktemp -d) || exit 2
trap 'rm -rf -- "$dir"' EXIT

# Copies at the paths in the repo give the diff header the correct path.
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

# Colour, when the output goes to a terminal, or a colour config asks for it.
# The inner git writes to a file, so it finds no terminal itself.
if [ -t 1 ]; then
  want=$(git config --get-colorbool color.diff true)
else
  want=$(git config --get-colorbool color.diff false)
fi
colourflag=--no-color
if [ "$want" = true ]; then
  colourflag=--color=always
fi

# --no-prefix: the file paths already start with a/ and b/, so git adds none.
rc=0
git diff --no-index --no-ext-diff "$colourflag" --no-prefix -- "$old" "$new"
rc=$?
[ "$rc" -le 1 ] || exit 2
exit "$rc"
