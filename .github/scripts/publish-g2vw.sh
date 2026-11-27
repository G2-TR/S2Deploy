#!/usr/bin/env bash
set -euo pipefail
umask 022

root=$1
archive=$2
base_url=$3
version=$4
[[ "$version" =~ ^[0-9]+-[0-9]+$ ]]
site="$root/Page"
test -s "$site/static/config.js"
mkdir -p "$root/g2vw-releases"
release=$(mktemp -d "$root/g2vw-releases/build.XXXXXXXX")
stage="$release/stage"
mkdir -p "$stage" "$release/previous"
tar -xzf "$archive" -C "$stage"
[[ -z "$(find "$stage" ! -type f ! -type d -print)" ]]
shopt -s nullglob
entries=("$stage"/module/*/index.html)
(( ${#entries[@]} > 0 ))

verify() {
  curl -fsS --compressed --max-time 20 "$base_url/$1?release=$version" -o "$release/probe"
  cmp "$release/probe" "$2"
}

# Only these immediate directories and module entries belong to this release.
ensure_dir() {
  [[ ! -L "$1" ]] && { [[ -d "$1" ]] || mkdir -- "$1"; }
}

ensure_dir "$site/module"
for entry in "${entries[@]}"; do
  name=${entry%/index.html}
  name=${name##*/}
  [[ "$name" =~ ^[A-Za-z0-9_-]+$ ]]
  live="$site/module/$name"
  [[ ! -L "$live" && ( ! -e "$live" || -d "$live" ) ]]
  [[ ! -L "$live/index.html" ]]
  if [[ -e "$live/index.html" ]]; then
    [[ -f "$live/index.html" ]]
    cp -p -- "$live/index.html" "$release/previous/$name.html"
  fi
done

# Retain previous assets for browsers that still have an older index open.
for kind in script assets; do
  dest="$site/$kind/g2vw"
  ensure_dir "$site/$kind"
  ensure_dir "$dest"
  source_dir="$stage/$kind/g2vw/$version"
  [[ -d "$source_dir" && ! -L "$dest/$version" ]]
  if [[ -d "$dest/$version" ]]; then
    diff -qr "$source_dir" "$dest/$version" >/dev/null
  else
    [[ ! -e "$dest/$version" ]]
    mv -- "$source_dir" "$dest/$version"
  fi
  while IFS= read -r -d '' file; do
    verify "${file#"$site"/}" "$file"
  done < <(find "$dest/$version" -type f -print0)
done
verify static/config.js "$site/static/config.js"

# Copy beside the live entry so each rename stays on one filesystem.
replace_index() {
  local next
  next=$(mktemp "$2/.g2vw.XXXXXXXX") || return 1
  if ! install -m 644 "$1" "$next" || ! mv -f -- "$next" "$2/index.html"; then
    rm -f -- "$next"
    return 1
  fi
}

changed=()
rollback() {
  local code=$? name
  trap - EXIT HUP INT TERM
  if (( code != 0 )); then
    for name in "${changed[@]}"; do
      if [[ -f "$release/previous/$name.html" ]]; then
        replace_index "$release/previous/$name.html" "$site/module/$name" || echo "Rollback failed for $name" >&2
      else
        rm -f -- "$site/module/$name/index.html"
      fi
    done
  fi
  rm -f -- "$release/probe"
  exit "$code"
}
trap rollback EXIT
trap 'exit 1' HUP INT TERM

for entry in "${entries[@]}"; do
  name=${entry%/index.html}
  name=${name##*/}
  ensure_dir "$site/module/$name"
  changed+=("$name")
  replace_index "$entry" "$site/module/$name"
  verify "module/$name/index.html" "$entry"
  if [[ "$name" == d2611031602 ]]; then
    verify "module/$name/P002.html" "$entry"
  fi
done
echo "Verified ${#entries[@]} module entries. Previous entries: $release/previous"
