#!/bin/sh
# Publishes GPU temperature and power draw through node_exporter's textfile
# collector.
#
# node_exporter has no idea a GPU exists, and the status collector only ever
# talks to node_exporter — so the numbers that are missing get written to a
# .prom file the exporter already picks up. Runs from ol1n-gpu-textfile.timer.
set -eu

DIR="${TEXTFILE_DIR:-/var/lib/prometheus/node-exporter}"

# Query first: if nvidia-smi fails, the old file is left alone and goes stale,
# which the collector notices — better than publishing an empty file.
readings="$(nvidia-smi --query-gpu=index,temperature.gpu,power.draw --format=csv,noheader,nounits)"

# The exporter only reads *.prom, so the dotfile is invisible until the rename.
tmp="$(mktemp "$DIR/.gpu.prom.XXXXXX")"
trap 'rm -f "$tmp"' EXIT

{
    echo '# HELP node_gpu_temp_celsius GPU temperature as reported by nvidia-smi.'
    echo '# TYPE node_gpu_temp_celsius gauge'
    echo "$readings" | awk -F', *' '$2 ~ /^[0-9.]+$/ {
        printf "node_gpu_temp_celsius{gpu=\"%s\"} %s\n", $1, $2
    }'
    echo '# HELP node_gpu_power_watts GPU power draw as reported by nvidia-smi.'
    echo '# TYPE node_gpu_power_watts gauge'
    echo "$readings" | awk -F', *' '$3 ~ /^[0-9.]+$/ {
        printf "node_gpu_power_watts{gpu=\"%s\"} %s\n", $1, $3
    }'
} > "$tmp"

chmod 644 "$tmp"
mv "$tmp" "$DIR/gpu.prom"
