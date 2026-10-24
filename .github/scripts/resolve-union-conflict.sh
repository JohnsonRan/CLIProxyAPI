#!/usr/bin/env bash
# Resolve one known textual conflict by keeping both sides verbatim, ours first.
#
# Usage: resolve-union-conflict.sh <file> <ours-sha256> <theirs-sha256>
#
# The file must hold exactly one conflict hunk whose two sides hash to the
# given values (sha256 of the side's lines, each terminated by a newline).
# Anything else fails closed: an unexpected file, a second hunk, a diff3 base
# section, or a changed side. Use it only where concatenating both sides is
# the reviewed resolution (two independent additions at the same spot).
set -euo pipefail

file=$1
want_ours=$2
want_theirs=$3

conflicted=$(git diff --name-only --diff-filter=U)
if [[ "${conflicted}" != "${file}" ]]; then
  echo "::error::expected only ${file} to conflict, got: ${conflicted//$'\n'/ }"
  exit 1
fi

hunks=$(grep -c '^<<<<<<< ' "${file}" || true)
if [[ "${hunks}" != 1 ]] || grep -q '^||||||| ' "${file}"; then
  echo "::error::${file}: expected exactly one two-way conflict hunk, got ${hunks}"
  exit 1
fi

side() {
  awk -v want="$1" '
    /^<<<<<<< /           { s = 1; next }
    /^=======$/ && s == 1 { s = 2; next }
    /^>>>>>>> /           { s = 0; next }
    s == want
  ' "${file}"
}
got_ours=$(side 1 | sha256sum | cut -d' ' -f1)
got_theirs=$(side 2 | sha256sum | cut -d' ' -f1)
if [[ "${got_ours}" != "${want_ours}" || "${got_theirs}" != "${want_theirs}" ]]; then
  echo "::error::${file}: conflict sides changed (ours ${got_ours}, theirs ${got_theirs}); re-review the resolution"
  exit 1
fi

tmp=$(mktemp)
awk '
  /^<<<<<<< /           { s = 1; next }
  /^=======$/ && s == 1 { s = 2; next }
  /^>>>>>>> /           { s = 0; printf "%s", ours; printf "\n"; printf "%s", theirs; next }
  s == 1 { ours = ours $0 "\n"; next }
  s == 2 { theirs = theirs $0 "\n"; next }
  { print }
' "${file}" > "${tmp}"
mv "${tmp}" "${file}"
git add -- "${file}"
echo "Resolved ${file} by keeping both sides"
