#!/usr/bin/env bash
# Run llama-server on a Google Colab GPU and expose it to Komet.
#
# In a Colab notebook (Runtime -> Change runtime type -> T4 GPU):
#
#   !git clone https://github.com/<you>/komet && cd komet
#   !bash scripts/colab.sh install          # download prebuilt llama.cpp with CUDA (~1 min)
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
# Prebuilt release to download; set LLAMA_TAG to another bNNNN to change it,
# or BUILD_FROM_SOURCE=1 to skip the download and compile (slow on Colab).
LLAMA_TAG=${LLAMA_TAG:-b11696}
REL=https://github.com/ggml-org/llama.cpp/releases/download/$LLAMA_TAG
BIN_DIR=$DIR/bin
BIN=$BIN_DIR/llama-server
RUN=$DIR/run
export LD_LIBRARY_PATH="$BIN_DIR:/usr/local/cuda/lib64:${LD_LIBRARY_PATH:-}"

log() { printf '\033[1m[colab]\033[0m %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
has_gpu() { nvidia-smi >/dev/null 2>&1; }
missing_libs() { ldd "$BIN" 2>/dev/null | grep 'not found' || true; }

install() {
	if [ -x "$BIN" ] && "$BIN" --version >/dev/null 2>&1; then log "llama-server ready: $BIN"; return; fi
	mkdir -p "$DIR"
	if [ "${BUILD_FROM_SOURCE:-0}" != 1 ] && install_prebuilt; then return; fi
	build_from_source
}

# Official release binaries: ~165 MB download instead of a long nvcc build.
install_prebuilt() {
	flavor=ubuntu-x64
	has_gpu && flavor=ubuntu-cuda-12.8-x64
	tmp=$(mktemp -d)
	log "downloading prebuilt llama.cpp $LLAMA_TAG ($flavor)"
	curl -fL --retry 3 -o "$tmp/llama.tar.gz" "$REL/llama-$LLAMA_TAG-bin-$flavor.tar.gz" || { rm -rf "$tmp"; return 1; }
	tar -xzf "$tmp/llama.tar.gz" -C "$tmp"
	rm -rf "$BIN_DIR"
	mv "$tmp/llama-$LLAMA_TAG" "$BIN_DIR"
	rm -rf "$tmp"

	# Colab ships the CUDA 12 runtime in /usr/local/cuda; fetch the bundled
	# cuBLAS/cudart (~570 MB) only if the system copy is missing.
	if has_gpu && [ -n "$(missing_libs)" ]; then
		log "system CUDA libraries missing; downloading the CUDA runtime bundle"
		tmp=$(mktemp -d)
		curl -fL --retry 3 -o "$tmp/cudart.tar.gz" "$REL/cudart-llama-$LLAMA_TAG-bin-$flavor.tar.gz" &&
			tar -xzf "$tmp/cudart.tar.gz" -C "$tmp" && cp "$tmp"/cudart-*/*.so* "$BIN_DIR"/
		rm -rf "$tmp"
	fi
	if [ -n "$(missing_libs)" ] || ! "$BIN" --version >/dev/null 2>&1; then
		missing_libs >&2
		log "prebuilt binary does not run here; falling back to a source build"
		rm -rf "$BIN_DIR"
		return 1
	fi
	log "installed $BIN"
}

build_from_source() {
	command -v cmake >/dev/null || { apt-get update -qq && apt-get install -y -qq cmake build-essential git; }
	[ -d "$DIR/llama.cpp" ] || git clone --depth 1 https://github.com/ggml-org/llama.cpp "$DIR/llama.cpp"
	export PATH="$PATH:/usr/local/cuda/bin"
	flags="-DLLAMA_CURL=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF"
	if command -v nvcc >/dev/null && has_gpu; then
		# "native" compiles kernels only for the attached GPU instead of every arch.
		flags="$flags -DGGML_CUDA=ON -DCMAKE_CUDA_ARCHITECTURES=native"
		log "building with CUDA for $(nvidia-smi --query-gpu=name --format=csv,noheader | head -1) (10+ min)"
	else
		log "no GPU runtime detected; building CPU-only (switch runtime to GPU for real speed)"
	fi
	cd "$DIR/llama.cpp"
	cmake -B build $flags -DCMAKE_BUILD_TYPE=Release
	cmake --build build -j"$(nproc)" --target llama-server
	rm -rf "$BIN_DIR"
	ln -s "$DIR/llama.cpp/build/bin" "$BIN_DIR"
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
