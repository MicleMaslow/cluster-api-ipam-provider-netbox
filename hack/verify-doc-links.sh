#!/usr/bin/env bash

# Copyright 2026 The cluster-api-ipam-provider-netbox Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Checks that every relative link and anchor in README.md, CONTRIBUTING.md and docs/**/*.md (as git lists them, so
# ignored files are left out) resolves: the file or directory exists and, for a .md target with #anchor, a heading
# makes that anchor.
# Anchors follow GitHub: lowercase, spaces to '-', everything but letters, digits, '-' and '_' dropped, a repeated
# heading gets -1, -2. External links (scheme://, mailto:) are not checked. Needs bash, git, awk and grep.
# Limitation: ASCII headings only (non-ASCII letters are dropped, GitHub keeps them); html anchors are not read.
set -o errexit
set -o nounset
set -o pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
export LC_ALL=C

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# anchors FILE: the anchors of the headings of FILE outside fenced code blocks, one per line.
anchors() {
  awk '
    /^[ \t]*(```|~~~)/ { fence = !fence; next }
    fence || !/^#+[ \t]/ { next }
    {
      sub(/^#+[ \t]+/, ""); sub(/[ \t]+#*[ \t]*$/, "")
      gsub(/\]\([^)]*\)/, ""); gsub(/[`*[]/, "")
      s = tolower($0); gsub(/[^a-z0-9 _-]/, "", s); gsub(/ /, "-", s)
      n = seen[s]++
      print n ? s "-" n : s
    }' "$1"
}

# links FILE: "line<TAB>target" of every inline link of FILE outside fenced code blocks and inline code.
links() {
  awk '
    /^[ \t]*(```|~~~)/ { fence = !fence; next }
    fence { next }
    {
      s = $0; gsub(/`[^`]*`/, "", s)
      while (match(s, /\]\([^)]*\)/)) {
        t = substr(s, RSTART + 2, RLENGTH - 3)
        sub(/[ \t].*$/, "", t); gsub(/^<|>$/, "", t)
        if (t != "") print NR "\t" t
        s = substr(s, RSTART + RLENGTH)
      }
    }' "$1"
}

has_anchor() {
  local cache=$tmp/${1//\//_}
  [ -f "$cache" ] || anchors "$1" >"$cache"
  grep -qxF -- "$2" "$cache"
}

files=()
while IFS= read -r f; do files+=("$f"); done < <(git ls-files --cached --others --exclude-standard README.md CONTRIBUTING.md 'docs/*.md')

bad=0 total=0
for f in "${files[@]}"; do
  while IFS=$'\t' read -r line target; do
    case $target in *://* | mailto:*) continue ;; esac
    total=$((total + 1))
    path=${target%%#*} anchor=''
    [[ $target != *#* ]] || anchor=${target#*#}
    case $path in
      '') dest=$f ;;
      /*) dest=.$path ;;
      *) dest=$(dirname "$f")/$path ;;
    esac
    if [ ! -e "$dest" ]; then
      echo "$f:$line: $target: no such file or directory" >&2
      bad=$((bad + 1))
    elif [ -n "$anchor" ] && [ -f "$dest" ] && [[ $dest == *.md ]] && ! has_anchor "$dest" "$anchor"; then
      echo "$f:$line: $target: no heading makes the anchor #$anchor in $dest" >&2
      bad=$((bad + 1))
    fi
  done < <(links "$f")
done

if [ "$bad" -ne 0 ]; then
  echo "$bad broken link(s) of $total" >&2
  exit 1
fi
echo "doc links ok: $total links in ${#files[@]} files"
