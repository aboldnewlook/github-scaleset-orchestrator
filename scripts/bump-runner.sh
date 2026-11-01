#!/usr/bin/env bash
# Pins the GitHub Actions runner gso downloads and bakes into its image.
#   scripts/bump-runner.sh            # latest actions/runner release
#   scripts/bump-runner.sh 2.338.2    # a specific release
# Digests come from the release notes' <!-- BEGIN SHA <platform> --> markers.
set -euo pipefail
cd "$(dirname "$0")/.."
tag="latest"
if [ $# -ge 1 ]; then tag="tags/v${1#v}"; fi
notes=$(gh api "repos/actions/runner/releases/$tag" --jq '.tag_name + "\n" + .body')
version=$(printf '%s\n' "$notes" | head -n1 | sed 's/^v//')
sha() { printf '%s\n' "$notes" | sed -n "s/.*<!-- BEGIN SHA $1 -->\([0-9a-f]\{64\}\)<!-- END SHA $1 -->.*/\1/p" | head -n1; }
tmp=$(mktemp)
jq -n --arg v "$version" \
  --arg lx "$(sha linux-x64)" --arg la "$(sha linux-arm64)" \
  --arg ox "$(sha osx-x64)" --arg oa "$(sha osx-arm64)" \
  '{version: $v, sha256: {"linux-x64": $lx, "linux-arm64": $la, "osx-x64": $ox, "osx-arm64": $oa}}
   | if ([.sha256[]] | all(test("^[0-9a-f]{64}$"))) then . else error("a digest is missing from the release notes") end' > "$tmp"
mv "$tmp" runner/pin.json
echo "runner/pin.json -> $version"
