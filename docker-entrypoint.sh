#!/bin/sh
set -eu

state_dir=${STATE_DIR:-/var/lib/opencode2api}
config_path=${CONFIG_PATH:-$state_dir/config.json}
config_seed_path=${CONFIG_SEED_PATH:-}
listen_address=${LISTEN_ADDRESS:-0.0.0.0:8080}
webui_listen_address=${WEBUI_LISTEN_ADDRESS:-0.0.0.0:8081}

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$state_dir"
    chown opencode2api:opencode2api "$state_dir"
    exec su-exec opencode2api:opencode2api "$0" "$@"
fi

if [ "$#" -gt 0 ]; then
    exec "$@"
fi

if [ ! -f "$config_path" ]; then
    mkdir -p "$(dirname "$config_path")"
    if [ -n "$config_seed_path" ] && [ -f "$config_seed_path" ]; then
        cp "$config_seed_path" "$config_path"
        printf '%s\n' "Initialized $config_path from $config_seed_path."
    else
        cp /app/config.example.json "$config_path"
        printf '%s\n' \
            "config.json not found; created $config_path. Set API keys or enable anonymous mode, and change the WebUI password before use."
    fi
fi

exec /usr/local/bin/opencode2api \
    -config "$config_path" \
    -listen "$listen_address" \
    -web-listen "$webui_listen_address"
