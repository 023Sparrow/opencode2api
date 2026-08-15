#!/bin/sh
set -eu

app_dir=${APP_DIR:-/app}
state_dir=${STATE_DIR:-/var/lib/opencode2api}
config_path=${CONFIG_PATH:-$app_dir/config.json}
listen_address=${LISTEN_ADDRESS:-0.0.0.0:8080}
webui_listen_address=${WEBUI_LISTEN_ADDRESS:-0.0.0.0:8081}
binary_path=$state_dir/opencode2api
fingerprint_path=$state_dir/source.sha256

mkdir -p "$state_dir"

if [ -f "$config_path" ]; then
    active_config=$config_path
else
    generated_config=$state_dir/config.json
    if [ ! -f "$generated_config" ]; then
        cp "$app_dir/config.example.json" "$generated_config"
    fi
    active_config=$generated_config
    printf '%s\n' "config.json not found; using a persistent generated copy. Set real API keys and change the WebUI password before use."
fi

source_fingerprint() {
    {
        go version
        go env GOOS GOARCH CGO_ENABLED
        find "$app_dir" \
            -path "$app_dir/.git" -prune -o \
            -type f \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' -o -path "$app_dir/webui/*" \) -print \
            | LC_ALL=C sort \
            | while IFS= read -r source_file; do
                sha256sum "$source_file"
            done
    } | sha256sum | awk '{print $1}'
}

current_fingerprint=$(source_fingerprint)
stored_fingerprint=""
if [ -f "$fingerprint_path" ]; then
    stored_fingerprint=$(cat "$fingerprint_path")
fi

if [ ! -x "$binary_path" ] || [ "$current_fingerprint" != "$stored_fingerprint" ]; then
    printf '%s\n' "Source changed or no cached binary exists; building opencode2api..."
    temporary_binary=$state_dir/opencode2api.new
    cd "$app_dir"
    go build -trimpath -o "$temporary_binary" ./
    mv "$temporary_binary" "$binary_path"
    printf '%s\n' "$current_fingerprint" > "$fingerprint_path"
    printf '%s\n' "Build completed and cached in the persistent Docker volume."
else
    printf '%s\n' "Source is unchanged; starting the cached opencode2api binary."
fi

exec "$binary_path" -config "$active_config" -listen "$listen_address" -web-listen "$webui_listen_address"
