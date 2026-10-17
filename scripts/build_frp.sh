#!/bin/bash
set -euo pipefail
# The module is authenticated by Go's checksum database and pinned below.
go_bin=${GO:-go}
version=v0.71.1
module=github.com/fatedier/frp
expected='h1:ZxKvRjQO19Y6XAVGJh3kYHQ6Jp9LDoPhUEtSN79Txk4='
project=$(cd -- "$(dirname -- "$0")/.." && pwd)
output=${1:-"$project/dist/tools"}
mkdir -p "$output"
output=$(cd -- "$output" && pwd)
metadata=$("$go_bin" mod download -json "$module@$version")
module_dir=$(python3 -c 'import json,sys; m=json.load(sys.stdin); assert m.get("Sum")==sys.argv[1], "frp module checksum mismatch"; print(m["Dir"])' "$expected" <<<"$metadata")
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOBIN="$output" "$go_bin" install -p=2 -trimpath -tags noweb \
  "$module/cmd/frpc@$version" "$module/cmd/frps@$version"
install -m 644 "$module_dir/LICENSE" "$output/FRP-LICENSE"
printf '%s\n' "$version" > "$output/FRP_VERSION"
echo "Built pinned frpc/frps $version in $output"
