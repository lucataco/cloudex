#!/bin/bash
# Bundles the API Worker into the CLI's embedded assets, so `cloudex deploy`
# can upload it without the source tree. Run before `go build`.
#   scripts/bundle-api.sh [version]
set -euo pipefail
version=${1:-dev}
root=$(cd "$(dirname "$0")/.." && pwd)
out="$root/go/internal/deploy/assets"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The dashboard first: the API embeds its files (apps/api/src/dashboard.ts).
(cd "$root/apps/dashboard" && node build.mjs >/dev/null)

cd "$root/apps/api"
# --containers-rollout none skips building the image; deploy copies a published one.
pnpm exec wrangler deploy --dry-run --minify --containers-rollout none --outdir "$tmp" \
  --define "CLOUDEX_VERSION:\"$version\"" >/dev/null
cp "$tmp/index.js" "$out/worker.js"
node -e '
  const fs = require("fs");
  const src = fs.readFileSync("wrangler.jsonc", "utf8").replace(/^\s*\/\/.*$/gm, "");
  const { compatibility_date, compatibility_flags = [] } = JSON.parse(src);
  fs.writeFileSync(process.argv[1], JSON.stringify({ compatibility_date, compatibility_flags, version: process.argv[2] }, null, 2) + "\n");
' "$out/meta.json" "$version"
echo "bundled API $version ($(wc -c < "$out/worker.js" | tr -d ' ') bytes) into go/internal/deploy/assets"
