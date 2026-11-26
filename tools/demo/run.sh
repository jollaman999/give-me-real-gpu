#!/usr/bin/env bash
# Records the demo GIF of the settings UI: builds give-me-real-gpu, starts it on loopback ports with an empty
# data directory, fills in settings and three dry-run sinks through the settings API, drives the UI in headless
# Chrome with record.mjs and turns the frames into a GIF. Everything it started is stopped on the way out.
#
#   tools/demo/run.sh [output.gif]
#
# The output defaults to docs/demo.gif (docs/demo.ko.gif with DEMO_LANG=ko). See tools/demo/README.md for the scenes,
# the captions and the environment variables.
set -euo pipefail

DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$DEMO_DIR/../.." && pwd)"
DEMO_LANG="${DEMO_LANG:-en}"
case "$DEMO_LANG" in en | ko) ;; *) echo "error: DEMO_LANG must be en or ko" >&2; exit 1 ;; esac
export DEMO_LANG
if [ "$DEMO_LANG" = ko ]; then DEFAULT_OUT="$REPO_DIR/docs/demo.ko.gif"; CHROME_LANG=ko-KR; else DEFAULT_OUT="$REPO_DIR/docs/demo.gif"; CHROME_LANG=en-US; fi
OUT="${1:-$DEFAULT_OUT}"
LISTEN_PORT="${DEMO_LISTEN_PORT:-27700}"
UI_PORT="${DEMO_UI_PORT:-27780}"
CHROME="${CHROME:-google-chrome}"
NODE="${NODE:-node}"
WIDTH="${DEMO_WIDTH:-960}"
MAX_BYTES="${DEMO_MAX_BYTES:-6291456}"
KEEP_WORK="${DEMO_KEEP_WORK:-0}"

log() { printf '[demo] %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

for tool in go curl ffmpeg ffprobe "$NODE" "$CHROME"; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool not found"
done

port_in_use() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
for p in "$LISTEN_PORT" "$UI_PORT"; do
	case "$p" in
	15000 | 15080) die "port $p is the default of a running emulator; pick another one" ;;
	esac
	if port_in_use "$p"; then
		die "port $p on 127.0.0.1 is already in use; set DEMO_LISTEN_PORT / DEMO_UI_PORT"
	fi
done

WORK="$(mktemp -d "${DEMO_WORK_PARENT:-${TMPDIR:-/tmp}}/gmrg-demo.XXXXXX")"
APP_PID=""
CHROME_PID=""

stop_pid() {
	local pid="$1"
	[ -n "$pid" ] || return 0
	kill -0 "$pid" 2>/dev/null || return 0
	kill "$pid" 2>/dev/null || true
	for _ in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || return 0
		sleep 0.1
	done
	kill -9 "$pid" 2>/dev/null || true
	wait "$pid" 2>/dev/null || true
}

cleanup() {
	local rc=$?
	stop_pid "$CHROME_PID"
	stop_pid "$APP_PID"
	if [ "$KEEP_WORK" = 1 ] || [ "$rc" -ne 0 ]; then
		log "work files kept in $WORK"
	else
		rm -rf -- "$WORK"
	fi
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

log "building into $WORK"
VERSION="${DEMO_VERSION:-$(git -C "$REPO_DIR" describe --tags --always 2>/dev/null || echo dev)}"
(cd "$REPO_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=$VERSION" -o "$WORK/give-me-real-gpu" ./cmd/give-me-real-gpu)

log "starting give-me-real-gpu (OpenStack API 127.0.0.1:$LISTEN_PORT, UI 127.0.0.1:$UI_PORT)"
"$WORK/give-me-real-gpu" -data "$WORK/data" -listen "127.0.0.1:$LISTEN_PORT" -ui-listen "127.0.0.1:$UI_PORT" \
	>"$WORK/app.log" 2>&1 &
APP_PID=$!

UI="http://127.0.0.1:$UI_PORT"
for i in $(seq 1 100); do
	if curl -fsS -o /dev/null "$UI/api/v1/status" 2>/dev/null; then
		break
	fi
	kill -0 "$APP_PID" 2>/dev/null || die "give-me-real-gpu exited; see $WORK/app.log"
	[ "$i" -lt 100 ] || die "give-me-real-gpu did not answer in 10s; see $WORK/app.log"
	sleep 0.1
done

api() {
	local method="$1" path="$2" body="$3"
	curl -fsS --fail-with-body -o /dev/null -X "$method" -H 'Content-Type: application/json' \
		--data-binary "$body" "$UI/api/v1$path" || die "$method $path failed"
}

log "seeding settings and sinks"
api PUT /settings '{
  "advertise_host": "192.0.2.10",
  "openstack_password": "<change-me>",
  "token_ttl": "1h",
  "interval": "10s",
  "seed": 1,
  "xid_rate": 0.002,
  "dry_run": true
}'
api POST /sinks '{
  "name": "site-a",
  "type": "influxdb",
  "influx_version": "v1",
  "url": "http://192.0.2.21:8086",
  "db": "gpu",
  "credential_id": "0b7d6a52-3c1e-4f8a-9d2b-6e5f4a3c2b1a",
  "enabled": true
}'
api POST /sinks '{
  "name": "site-b",
  "type": "influxdb",
  "influx_version": "v2",
  "url": "http://198.51.100.22:8086",
  "org": "example",
  "db": "gpu-metrics",
  "token": "<change-me>",
  "credential_id": "7c9e2f14-8a3b-4d6c-b1e5-2f0a9d8c7b6e",
  "enabled": true,
  "tag_rules": [
    {"action": "rename", "key": "host", "new_key": "hostname"},
    {"action": "drop", "key": "pstate"},
    {"action": "add", "key": "site", "value": "site-b"}
  ]
}'
api POST /sinks '{
  "name": "site-c",
  "type": "otlp",
  "url": "http://203.0.113.23:4318/v1/metrics",
  "enabled": true
}'

log "starting headless Chrome"
mkdir -p "$WORK/chrome" "$WORK/frames"
"$CHROME" --headless --remote-debugging-port=0 --user-data-dir="$WORK/chrome" \
	--window-size=1280,800 --lang="$CHROME_LANG" --hide-scrollbars --force-color-profile=srgb \
	--no-first-run --no-default-browser-check --disable-extensions --disable-sync \
	--disable-background-networking --password-store=basic --use-mock-keychain \
	about:blank >"$WORK/chrome.log" 2>&1 &
CHROME_PID=$!
for i in $(seq 1 100); do
	[ -s "$WORK/chrome/DevToolsActivePort" ] && break
	kill -0 "$CHROME_PID" 2>/dev/null || die "Chrome exited; see $WORK/chrome.log"
	[ "$i" -lt 100 ] || die "Chrome did not open its DevTools port in 10s"
	sleep 0.1
done
DEVTOOLS_PORT="$(head -n 1 "$WORK/chrome/DevToolsActivePort")"

NODE_FLAGS=()
if ! "$NODE" -e 'process.exit(typeof WebSocket === "function" ? 0 : 1)'; then
	NODE_FLAGS+=(--experimental-websocket)
fi

log "recording"
"$NODE" "${NODE_FLAGS[@]}" "$DEMO_DIR/record.mjs" \
	--devtools-port "$DEVTOOLS_PORT" --url "$UI/ui/" --frames "$WORK/frames" --fps "${DEMO_FPS:-10}"

log "making the GIF"
mkdir -p "$(dirname "$OUT")"
# Lower the frame rate and the colors first; only when that is not enough, make the GIF narrower.
fit=0
for width in "$WIDTH" $((WIDTH * 7 / 8)) $((WIDTH * 3 / 4)); do
	for step in "${DEMO_FPS:-10}:128" 8:128 8:64 6:64 5:48; do
		fps="${step%:*}"
		colors="${step#*:}"
		ffmpeg -v error -y -f concat -safe 0 -i "$WORK/frames/frames.txt" \
			-vf "fps=$fps,scale=$width:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=$colors:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
			-loop 0 "$WORK/demo.gif"
		size="$(stat -c %s "$WORK/demo.gif")"
		log "${width}px, $fps fps, $colors colors: $size bytes"
		if [ "$size" -le "$MAX_BYTES" ]; then
			fit=1
			break 2
		fi
	done
done
[ "$fit" = 1 ] || log "warning: the GIF is still larger than $MAX_BYTES bytes"

cp "$WORK/demo.gif" "$OUT"
duration="$(ffprobe -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 "$OUT")"
gif_frames="$(ffprobe -v error -count_frames -select_streams v:0 -show_entries stream=nb_read_frames -of default=noprint_wrappers=1:nokey=1 "$OUT")"
log "wrote $OUT: $(stat -c %s "$OUT") bytes, ${duration}s, ${width}px wide, $fps fps, $colors colors, $gif_frames GIF frames from $(find "$WORK/frames" -name 'f*.png' | wc -l) screenshots"
