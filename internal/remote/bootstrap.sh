#!/usr/bin/env bash
# Komet remote agent. Sent over SSH on stdin (`ssh host bash -s -- <cmd> ...`)
# and never installed permanently, so the remote side always matches the
# local Komet version. Everything persistent lives in $WORK (a volume that
# survives stop/start: /workspace on RunPod, the EBS root on AWS).
#
#   probe <work>                         -> one JSON line: hardware profile
#   install <work> <engine> <cmd>        -> build/install the engine once
#   start <work> <engine> <bin> <model> <url> <port> <ctx> <ngl> <parallel>
#         <idle_sec> <max_up_sec> <stop_mode> <pod_id> <api_base>
#                                        -> one JSON line; server + watchdog up
#   stop <work>                          -> stop server and watchdog
#
# Secrets (inference API key, optional RunPod stop key) arrive as
# KOMET_API_KEY / KOMET_STOP_KEY lines prepended to this script by Komet,
# never on a command line.
set -uo pipefail

expand() { case "$1" in "~"|"~/"*) printf '%s' "$HOME${1#\~}" ;; *) printf '%s' "$1" ;; esac; }
jstr() { local s=${1//\\/\\\\}; s=${s//\"/\\\"}; printf '"%s"' "${s//$'\n'/ }"; }
die() { printf '{"error":%s}\n' "$(jstr "$*")"; exit 1; }

cmd=${1:-}; shift || true
WORK=$(expand "${1:-~/komet}"); shift || true
mkdir -p "$WORK/run" "$WORK/logs" "$WORK/models" || die "cannot create $WORK"
RUN="$WORK/run"

default_bin() { printf '%s' "$WORK/llama.cpp/build/bin/llama-server"; }

engine_present() { # engine bin
	if [ "$1" = vllm ]; then
		python3 -c 'import vllm' >/dev/null 2>&1 || command -v vllm >/dev/null 2>&1
	else
		[ -x "$2" ] || command -v "$2" >/dev/null 2>&1
	fi
}

alive() { [ -f "$1" ] && kill -0 "$(cat "$1")" 2>/dev/null; }

now_ms() { local n; n=$(date +%s%N 2>/dev/null); case "$n" in *N | "") echo $(( $(date +%s) * 1000 )) ;; *) echo $(( n / 1000000 )) ;; esac; }
secs_since() { awk -v a="$1" -v b="$(now_ms)" 'BEGIN { printf "%.1f", (b - a) / 1000 }'; }

healthy() { # port
	if command -v curl >/dev/null 2>&1; then
		curl -fsS -m 2 "http://127.0.0.1:$1/health" >/dev/null 2>&1
	else
		wget -q -T 2 -O /dev/null "http://127.0.0.1:$1/health" 2>/dev/null
	fi
}

case "$cmd" in
probe)
	engine=${1:-llama}; bin=$(expand "${2:-}"); [ -n "$bin" ] || bin=$(default_bin)
	gpus=""
	if command -v nvidia-smi >/dev/null 2>&1; then
		while IFS=, read -r name mem drv; do
			[ -n "$name" ] || continue
			gpus+="${gpus:+,}{\"name\":$(jstr "$(echo $name)"),\"vram_mb\":${mem//[!0-9]/},\"driver\":$(jstr "$(echo $drv)")}"
		done < <(nvidia-smi --query-gpu=name,memory.total,driver_version --format=csv,noheader,nounits 2>/dev/null)
	fi
	cuda=$(nvidia-smi 2>/dev/null | sed -n 's/.*CUDA Version: *\([0-9.]*\).*/\1/p' | head -1)
	ram=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo 2>/dev/null)
	disk=$(df -Pm "$WORK" 2>/dev/null | awk 'NR==2 {print $4}')
	present=false; engine_present "$engine" "$bin" && present=true
	models=""
	for f in "$WORK"/models/*; do [ -f "$f" ] && models+="${models:+,}$(jstr "$f")"; done
	printf '{"os":%s,"arch":%s,"cpus":%s,"ram_mb":%s,"disk_free_mb":%s,"gpus":[%s],"cuda":%s,"engine":%s,"engine_present":%s,"engine_bin":%s,"work_dir":%s,"models":[%s],"has_curl":%s}\n' \
		"$(jstr "$(uname -s)")" "$(jstr "$(uname -m)")" "$(nproc 2>/dev/null || echo 0)" "${ram:-0}" "${disk:-0}" \
		"$gpus" "$(jstr "$cuda")" "$(jstr "$engine")" "$present" "$(jstr "$bin")" "$(jstr "$WORK")" "$models" \
		"$(command -v curl >/dev/null && echo true || echo false)"
	;;

install)
	engine=${1:-llama}; custom=${2:-}
	log="$WORK/logs/install.log"
	if [ -n "$custom" ]; then
		(cd "$WORK" && bash -c "$custom") >"$log" 2>&1 || die "install command failed, see $log on the target"
	elif [ "$engine" = vllm ]; then
		python3 -m pip install -q vllm >"$log" 2>&1 || die "pip install vllm failed, see $log on the target"
	else
		{
			command -v cmake >/dev/null || { apt-get update -qq && apt-get install -y -qq cmake build-essential git; } || exit 1
			[ -d "$WORK/llama.cpp" ] || git clone --depth 1 https://github.com/ggml-org/llama.cpp "$WORK/llama.cpp" || exit 1
			cd "$WORK/llama.cpp" || exit 1
			flags=""; command -v nvcc >/dev/null || [ -x /usr/local/cuda/bin/nvcc ] && flags="-DGGML_CUDA=ON"
			export PATH="$PATH:/usr/local/cuda/bin"
			cmake -B build $flags -DCMAKE_BUILD_TYPE=Release -DLLAMA_CURL=OFF && cmake --build build -j"$(nproc)" --target llama-server
		} >"$log" 2>&1 || die "building llama.cpp failed, see $log on the target"
	fi
	printf '{"installed":true}\n'
	;;

start)
	engine=$1 bin=$(expand "$2") model=$(expand "$3") url=$4 port=$5 ctx=$6 ngl=$7 par=$8
	idle=$9 maxup=${10} stopmode=${11} pod=${12} apibase=${13}
	[ -n "$bin" ] || bin=$(default_bin)
	keyfile="$RUN/apikey"
	umask 077
	printf '%s' "${KOMET_API_KEY:?missing api key}" >"$keyfile"
	[ -n "${KOMET_STOP_KEY:-}" ] && printf '%s' "$KOMET_STOP_KEY" >"$RUN/stopkey"

	# Reuse a server that already runs this model on this port.
	if alive "$RUN/server.pid" && [ "$(cat "$RUN/server.model" 2>/dev/null)" = "$model:$port" ] && healthy "$port"; then
		reused=true
	else
		reused=false
		alive "$RUN/server.pid" && kill "$(cat "$RUN/server.pid")" 2>/dev/null && sleep 1
		dl_s=0
		if [ "$engine" = llama ] && [ ! -f "$model" ]; then
			[ -n "$url" ] || die "model $model is missing and no url is configured"
			t0=$(now_ms)
			mkdir -p "$(dirname "$model")"
			if command -v curl >/dev/null; then
				curl -fL --retry 3 -o "$model.part" "$url" >"$WORK/logs/download.log" 2>&1
			else
				wget -q -O "$model.part" "$url" >"$WORK/logs/download.log" 2>&1
			fi || die "model download failed, see $WORK/logs/download.log on the target"
			mv "$model.part" "$model"
			dl_s=$(secs_since "$t0") downloaded=true
		fi
		log="$WORK/logs/server.log"
		if [ "$engine" = vllm ]; then
			export HF_HOME="${HF_HOME:-$WORK/hf}"
			VLLM_API_KEY=$(cat "$keyfile") nohup setsid vllm serve "$model" --host 127.0.0.1 --port "$port" \
				--max-model-len "$ctx" --max-num-seqs "$par" >"$log" 2>&1 </dev/null &
		else
			engine_present llama "$bin" || die "llama-server not found at $bin"
			nohup setsid "$bin" -m "$model" --host 127.0.0.1 --port "$port" -c "$ctx" -ngl "$ngl" \
				-np "$par" --api-key-file "$keyfile" >"$log" 2>&1 </dev/null &
		fi
		echo $! >"$RUN/server.pid"
		echo "$model:$port" >"$RUN/server.model"
	fi

	# Watchdog: stops the server (and, if allowed, the machine) when no SSH
	# session has been connected for $idle seconds, or after $maxup seconds.
	# Idle is measured on the sshd port this session came in on, so a
	# laptop that disappears without saying goodbye still stops the bill.
	sshd_port=$(echo "${SSH_CONNECTION:-x x x 22}" | awk '{print $4}')
	cat >"$RUN/watchdog.sh" <<WD
#!/usr/bin/env bash
hexport=\$(printf '%04X' $sshd_port)
start=\$(date +%s); last=\$start
log() { echo "\$(date -u +%FT%TZ) \$*" >>"$WORK/logs/watchdog.log"; }
sessions() { cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk -v p=":\$hexport" '\$2 ~ p"\$" && \$4 == "01"' | wc -l; }
log "watching sshd port $sshd_port idle=${idle}s max_uptime=${maxup}s stop=$stopmode"
while sleep 15; do
	now=\$(date +%s)
	[ "\$(sessions)" -gt 0 ] && last=\$now
	reason=""
	[ \$((now - last)) -ge $idle ] && reason="no ssh session for ${idle}s"
	[ \$((now - start)) -ge $maxup ] && reason="max uptime ${maxup}s reached"
	[ -n "\$reason" ] || continue
	log "stopping: \$reason"
	[ -f "$RUN/server.pid" ] && kill \$(cat "$RUN/server.pid") 2>/dev/null
	rm -f "$RUN/server.pid" "$RUN/server.model"
	case "$stopmode" in
	aws) sudo -n shutdown -h now 2>/dev/null || shutdown -h now ;;
	runpod)
		if [ -s "$RUN/stopkey" ]; then
			curl -fsS -m 20 -X POST "$apibase/pods/$pod/action" -H "Authorization: Bearer \$(cat "$RUN/stopkey")" \
				-H 'Content-Type: application/json' -d '{"action":"stop"}' >>"$WORK/logs/watchdog.log" 2>&1 \
				&& log "pod stop requested" || log "pod stop request failed"
		fi ;;
	esac
	rm -f "$RUN/watchdog.pid"
	exit 0
done
WD
	chmod 700 "$RUN/watchdog.sh"
	alive "$RUN/watchdog.pid" && kill "$(cat "$RUN/watchdog.pid")" 2>/dev/null
	nohup setsid bash "$RUN/watchdog.sh" >/dev/null 2>&1 </dev/null &
	echo $! >"$RUN/watchdog.pid"

	# Wait until the server answers, so errors surface here with the log tail.
	t0=$(now_ms)
	until healthy "$port"; do
		if ! alive "$RUN/server.pid"; then
			die "server exited: $(tail -n 5 "$WORK/logs/server.log" 2>/dev/null | tr '\n' ' ')"
		fi
		[ $(( $(now_ms) - t0 )) -gt 900000 ] && die "server not healthy after 900s"
		sleep 1
	done
	printf '{"reused":%s,"downloaded":%s,"download_s":%s,"engine_ready_s":%s,"sshd_port":%s}\n' \
		"$reused" "${downloaded:-false}" "${dl_s:-0}" "$(secs_since "$t0")" "$sshd_port"
	;;

stop)
	for f in server watchdog; do alive "$RUN/$f.pid" && kill "$(cat "$RUN/$f.pid")" 2>/dev/null; done
	rm -f "$RUN/server.pid" "$RUN/server.model" "$RUN/watchdog.pid"
	printf '{"stopped":true}\n'
	;;

*) die "unknown command $cmd" ;;
esac
