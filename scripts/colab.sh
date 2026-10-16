#!/usr/bin/env bash
# Run llama-server on a Google Colab GPU and expose it to Komet.
#
# In a Colab notebook (Runtime -> Change runtime type -> T4 GPU):
#
#   !git clone https://github.com/<you>/komet && cd komet
#   !bash scripts/colab.sh install          # build llama.cpp with CUDA (~10 min first time)
#   !bash scripts/colab.sh model            # download the default GGUF model
#   !bash scripts/colab.sh serve            # start llama-server + HTTPS tunnel, print Komet config
#   !bash scripts/colab.sh stop
#
# Builds and models go to $KOMET_COLAB_DIR (default /content/komet-colab).
# Mount Google Drive and set KOMET_COLAB_DIR=/content/drive/MyDrive/komet-colab
# to keep them across sessions, so later sessions skip the build and download.
#
# The tunnel URL is public, so the server always requires an API key, and
# Komet should use it as the cloud tier (trust "public": secrets get redacted).
set -euo pipefail

DIR=${KOMET_COLAB_DIR:-/content/komet-colab}
MODEL_URL=${MODEL_URL:-https://huggingface.co/Qwen/Qwen2.5-Coder-7B-Instruct-GGUF/resolve/main/qwen2.5-coder-7b-instruct-q4_k_m.gguf}
MODEL_NAME=${MODEL_NAME:-qwen2.5-coder-7b}
MODEL=${MODEL:-$DIR/models/$(basename "$MODEL_URL")}
PORT=${PORT:-8080}
CTX=${CTX:-8192}
BIN=$DIR/llama.cpp/build/bin/llama-server
RUN=$DIR/run

log() { printf '\033[1m[colab]\033[0m %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

install() {
	if [ -x "$BIN" ]; then log "llama-server already built: $BIN"; return; fi
	mkdir -p "$DIR"
	command -v cmake >/dev/null || { apt-get update -qq && apt-get install -y -qq cmake build-essential git; }
	[ -d "$DIR/llama.cpp" ] || git clone --depth 1 https://github.com/ggml-org/llama.cpp "$DIR/llama.cpp"
	export PATH="$PATH:/usr/local/cuda/bin"
	flags="-DLLAMA_CURL=OFF"
	if command -v nvcc >/dev/null && nvidia-smi >/dev/null 2>&1; then
		# "native" builds only for the attached GPU, which is much faster than all archs.
		flags="$flags -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=native"
		log "building with CUDA for $(nvidia-smi --query-gpu=name --format=csv,noheader | head -1)"
	else
		log "no GPU runtime detected; building CPU-only (switch runtime to GPU for real speed)"
	fi
	cd "$DIR/llama.cpp"
	cmake -B build $flags -DCMAKE_BUILD_TYPE=Release
	cmake --build build -j"$(nproc)" --target llama-server
	log "built $BIN"
}

model() {
	if [ -s "$MODEL" ]; then log "model present: $MODEL"; return; fi
	mkdir -p "$(dirname "$MODEL")"
	log "downloading $MODEL_URL"
	curl -fL --retry 3 -o "$MODEL.part" "$MODEL_URL"
	mv "$MODEL.part" "$MODEL"
}

serve() {
	[ -x "$BIN" ] || die "llama-server not built; run: bash scripts/colab.sh install"
	[ -s "$MODEL" ] || die "model missing; run: bash scripts/colab.sh model"
	mkdir -p "$RUN"
	stop_procs

	key=${KOMET_API_KEY:-$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')}
	ngl=0; nvidia-smi >/dev/null 2>&1 && ngl=99
	nohup "$BIN" -m "$MODEL" --alias "$MODEL_NAME" -ngl "$ngl" -c "$CTX" \
		--host 127.0.0.1 --port "$PORT" --api-key "$key" >"$RUN/server.log" 2>&1 &
	echo $! >"$RUN/server.pid"
	log "loading model (log: $RUN/server.log)"
	for _ in $(seq 1 180); do
		curl -fs "http://127.0.0.1:$PORT/health" >/dev/null 2>&1 && break
		kill -0 "$(cat "$RUN/server.pid")" 2>/dev/null || { tail -20 "$RUN/server.log" >&2; die "llama-server exited"; }
		sleep 1
	done
	curl -fs "http://127.0.0.1:$PORT/health" >/dev/null || die "llama-server not healthy after 180s"

	cf=$DIR/cloudflared
	if [ ! -x "$cf" ]; then
		curl -fsSL -o "$cf" https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64
		chmod +x "$cf"
	fi
	nohup "$cf" tunnel --no-autoupdate --url "http://127.0.0.1:$PORT" >"$RUN/tunnel.log" 2>&1 &
	echo $! >"$RUN/tunnel.pid"
	url=""
	for _ in $(seq 1 60); do
		url=$(grep -o 'https://[a-z0-9-]*\.trycloudflare\.com' "$RUN/tunnel.log" | head -1 || true)
		[ -n "$url" ] && break
		sleep 1
	done
	[ -n "$url" ] || { tail -20 "$RUN/tunnel.log" >&2; die "tunnel did not come up"; }

	cat <<EOF

llama-server is up at $url

On your machine, run:
  export COLAB_API_KEY=$key

and set the "cloud" block in ~/.komet/config.json to:
  "cloud": {
    "enabled": true,
    "base_url": "$url/v1",
    "model": "$MODEL_NAME",
    "api_key_env": "COLAB_API_KEY",
    "price_in_per_mtok": 0,
    "price_out_per_mtok": 0,
    "json_mode": true
  }

Then: komet doctor
Keep this Colab tab open; the URL and key change every session.
EOF
}

stop_procs() {
	for p in tunnel server; do
		[ -f "$RUN/$p.pid" ] && kill "$(cat "$RUN/$p.pid")" 2>/dev/null || true
		rm -f "$RUN/$p.pid"
	done
}

case "${1:-}" in
install) install ;;
model) model ;;
serve) serve ;;
all) install; model; serve ;;
stop) stop_procs; log "stopped" ;;
*) echo "usage: $0 install|model|serve|all|stop" >&2; exit 2 ;;
esac
