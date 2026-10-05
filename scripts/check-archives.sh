#!/bin/sh
# Checks the release archives in dist/ that `make snapshot` (or CI's goreleaser --snapshot) built
# from HEAD: the four archives and their checksums, this machine's binary reports HEAD's commit,
# every archive carries the notices and docs byte for byte the committed files (DEV-98, DEV-183),
# every entry is owned by root/root (DEV-183), every relative link in a shipped Markdown file
# resolves inside the unpacked archive (DEV-192), and the cask has four urls and clears the
# quarantine. Runs with GNU tar (CI's ubuntu runner) and macOS's bsdtar (DEV-187).
set -eu

fail() { echo "::error::$*" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for target in darwin_arm64 darwin_amd64 linux_amd64 linux_arm64; do
  ls dist/devdash_*_"$target".tar.gz
done
if command -v sha256sum >/dev/null; then
  (cd dist && sha256sum -c checksums.txt)
else
  (cd dist && shasum -a 256 -c checksums.txt)
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64) arch=amd64 ;;
  aarch64) arch=arm64 ;;
  *) arch=$(uname -m) ;;
esac
tar -xzf dist/devdash_*_"${os}_$arch".tar.gz -C "$tmp" devdash
out=$("$tmp/devdash" version)
echo "$out"
sha=$(git rev-parse HEAD)
case "$out" in
  "devdash v"*"-snapshot."*" (commit $sha, built "*")") ;;
  *) fail "unexpected version output" ;;
esac

for archive in dist/devdash_*.tar.gz; do
  for f in THIRD_PARTY_LICENSES docs/usage.md docs/limitations.md docs/json-schema.md; do
    tar -xzOf "$archive" "$f" | cmp - "$f" || fail "$archive: $f missing or not the committed file"
  done

  # GNU tar prints owner/group as one column (root/root), bsdtar as two (root root).
  if tar -tzvf "$archive" | awk '!($2 == "root/root" || ($3 == "root" && $4 == "root"))' | grep .; then
    fail "$archive: entries above are not owned by root/root"
  fi

  # Inline links only: the shipped docs use no reference-style links.
  rm -rf "$tmp/x" && mkdir "$tmp/x" && tar -xzf "$archive" -C "$tmp/x"
  dead=$(cd "$tmp/x" && find . -name '*.md' | sort | while read -r md; do
    grep -oE '\]\([^)]+\)' "$md" | sed -e 's/^](//' -e 's/)$//' -e 's/ .*//' -e 's/#.*//' |
      grep -vE '^$|^[a-z]+:' | while read -r link; do
        [ -e "$(dirname "$md")/$link" ] || echo "${md#./} -> $link"
      done
  done)
  [ -z "$dead" ] || { echo "$dead"; fail "$archive: the links above leave the archive; make them absolute URLs"; }
done

cask=dist/homebrew/Casks/devdash.rb
cat "$cask"
test "$(grep -c '^ *url ' "$cask")" -eq 4 || fail "$cask: want 4 urls"
grep -q 'com.apple.quarantine' "$cask" || fail "$cask: no quarantine removal"
echo "archives ok"
