#!/usr/bin/env bash
set -euo pipefail

root=$1
archive=$2
base_url=$3
live="$root/Page/G2un"
releases="$root/g2un-releases"
mkdir -p "$releases"
release=$(mktemp -d "$releases/build.XXXXXXXX")
next="$release/next"
old="$release/previous"
mkdir -m 755 "$next"
tar -xzf "$archive" -C "$next"
test -s "$next/index.html"
test -s "$next/static/config.js"
asset=$(sed -n 's|.*src="/G2un/\(assets/[^"]*\.js\)".*|\1|p' "$next/index.html" | head -1)
test -n "$asset" && test -s "$next/$asset" || exit 1

# Preserve the site-specific config before moving the staged directory into service.
if [[ -e "$live/static/config.js" ]]; then
  cp -pL "$live/static/config.js" "$next/static/config.js"
fi

switched=false
rollback() {
  local rc=$?
  if (( rc != 0 )); then
    if [[ "$switched" == true ]]; then mv "$live" "$release/failed"; fi
    if [[ -e "$old" || -L "$old" ]]; then mv "$old" "$live"; fi
  fi
}
trap rollback EXIT
trap 'exit 1' HUP INT TERM
if [[ -e "$live" || -L "$live" ]]; then mv "$live" "$old"; fi
mv "$next" "$live"
switched=true

verify() {
  curl -fsS --compressed --max-time 15 "$base_url/$1?release=${release##*/}" -o "$release/probe"
  cmp "$release/probe" "$live/$2"
}
verify '' index.html
verify 'module/T2611120900/Pages/P001' index.html
verify "$asset" "$asset"
verify static/config.js static/config.js
rm -f "$release/probe"
echo "Verified G2un.work at $base_url/"
if [[ -e "$old" ]]; then echo "Previous version: $old"; fi
