#!/usr/bin/env bash
# Builds luau.wasm (docs/needletail.md §5.1): Luau's VM and compiler plus src/shim.cpp, compiled to
# wasm32-wasip1 with a pinned wasi-sdk. Luau raises errors as C++ exceptions, built on the
# WebAssembly exception-handling proposal (the standard one, exnref), which wazero runs.
#
#   internal/needletail/luau/wasm/build.sh [out.wasm]
#
# Downloads are checked against pinned sha256 sums before anything is extracted, into a cache
# ($NEEDLETAIL_WASM_CACHE, default ~/.cache/needletail-wasm). Nothing from the downloads is run
# except the pinned compiler. The output is reproducible: the same inputs and flags give the same
# bytes (TestModuleSHA256 pins the committed file's sum).
set -euo pipefail

WASI_SDK_TAG=wasi-sdk-34
WASI_SDK_VERSION=34.0
LUAU_TAG=0.740
LUAU_COMMIT=c0e346edd89066b44dca174c9f54ce84c746a540
LUAU_SHA256=419f96e4ecf5a0dd415a2625ea7ea9dc8a407e86d64b25e00936302b25a7afbd

case "$(uname -s)-$(uname -m)" in
Darwin-arm64) SDK_ASSET=wasi-sdk-$WASI_SDK_VERSION-arm64-macos.tar.gz SDK_SHA256=9c59398106b417f8f14913380fdf0097a8cc0ff4af9eb3ce0065a859e88d49e9 ;;
Darwin-x86_64) SDK_ASSET=wasi-sdk-$WASI_SDK_VERSION-x86_64-macos.tar.gz SDK_SHA256=87d27fa8adc68dee59bfbf2e22a6d34ef717c34d6bf1d8af2a56fc929d9ce0eb ;;
Linux-x86_64) SDK_ASSET=wasi-sdk-$WASI_SDK_VERSION-x86_64-linux.tar.gz SDK_SHA256=b761e3a0721dbae9c09a0059e5fdb2bf917d1b4a8a7b430fb3b5aafb0984b2c4 ;;
Linux-aarch64 | Linux-arm64) SDK_ASSET=wasi-sdk-$WASI_SDK_VERSION-arm64-linux.tar.gz SDK_SHA256=f7e243dff54d60bcc576e94d6166b69f410f2500ae4a9ceef34315be10e77971 ;;
*)
	echo "build.sh: no pinned wasi-sdk for $(uname -s)-$(uname -m)" >&2
	exit 1
	;;
esac

HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${1:-$HERE/luau.wasm}
CACHE=${NEEDLETAIL_WASM_CACHE:-$HOME/.cache/needletail-wasm}
mkdir -p "$CACHE"

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# fetch URL FILE SHA256: download once, then always verify.
fetch() {
	local url=$1 file=$2 want=$3
	if [ ! -f "$file" ]; then
		curl -fsSL --proto '=https' -o "$file.part" "$url"
		mv "$file.part" "$file"
	fi
	local got
	got=$(sha256 "$file")
	if [ "$got" != "$want" ]; then
		echo "build.sh: $file: sha256 $got, want $want" >&2
		rm -f "$file"
		exit 1
	fi
}

# unpack ARCHIVE DIR: extract into a new empty DIR (once; a finished extraction leaves .ok).
unpack() {
	local archive=$1 dir=$2
	if [ -f "$dir/.ok" ]; then return; fi
	rm -rf "$dir"
	mkdir -p "$dir"
	tar -xzf "$archive" -C "$dir" --strip-components=1
	touch "$dir/.ok"
}

fetch "https://github.com/WebAssembly/wasi-sdk/releases/download/$WASI_SDK_TAG/$SDK_ASSET" "$CACHE/$SDK_ASSET" "$SDK_SHA256"
fetch "https://github.com/luau-lang/luau/archive/refs/tags/$LUAU_TAG.tar.gz" "$CACHE/luau-$LUAU_TAG.tar.gz" "$LUAU_SHA256"
if command -v git >/dev/null; then
	# get-tar-commit-id reads only the header: gzip's SIGPIPE is expected
	got=$(gzip -dc "$CACHE/luau-$LUAU_TAG.tar.gz" 2>/dev/null | git get-tar-commit-id) || true
	if [ "$got" != "$LUAU_COMMIT" ]; then
		echo "build.sh: luau-$LUAU_TAG.tar.gz is commit $got, want $LUAU_COMMIT" >&2
		exit 1
	fi
fi
SDK=$CACHE/${SDK_ASSET%.tar.gz}
LUAU=$CACHE/luau-$LUAU_TAG
unpack "$CACHE/$SDK_ASSET" "$SDK"
unpack "$CACHE/luau-$LUAU_TAG.tar.gz" "$LUAU"

OBJ=$(mktemp -d "$CACHE/build.XXXXXX")
trap 'rm -rf "$OBJ"' EXIT

CXX=("$SDK/bin/clang++" --target=wasm32-wasip1 "--sysroot=$SDK/share/wasi-sysroot")
CXXFLAGS=(-O2 -std=c++17 -fwasm-exceptions -mllvm -wasm-use-legacy-eh=false
	-DNDEBUG -DLUA_USE_LONGJMP=0 -DLUAI_MAXCCALLS=200
	"-ffile-prefix-map=$LUAU=luau" "-ffile-prefix-map=$SDK=wasi-sdk" "-ffile-prefix-map=$HERE=shim"
	-Wno-deprecated-declarations
	"-I$LUAU/Common/include" "-I$LUAU/Ast/include" "-I$LUAU/Bytecode/include" "-I$LUAU/Compiler/include" "-I$LUAU/VM/include")

SOURCES=()
for lib in Common Ast Bytecode Compiler VM; do
	for f in "$LUAU/$lib"/src/*.cpp; do SOURCES+=("$f"); done
done
SOURCES+=("$HERE/src/shim.cpp")

objs=()
pids=()
for f in "${SOURCES[@]}"; do
	o="$OBJ/$(basename "$(dirname "$(dirname "$f")")")_$(basename "$f" .cpp).o"
	[ "$f" = "$HERE/src/shim.cpp" ] && o="$OBJ/shim.o"
	"${CXX[@]}" "${CXXFLAGS[@]}" -c "$f" -o "$o" &
	pids+=($!)
	objs+=("$o")
done
for p in "${pids[@]}"; do wait "$p"; done

# The stack goes first, so an overflow traps (out of bounds) instead of overwriting data; 1 MiB of
# it, and memory capped at 64 MiB, the host's limit.
"${CXX[@]}" -fwasm-exceptions -mexec-model=reactor -Wl,--stack-first -Wl,-z,stack-size=1048576 \
	-Wl,--max-memory=67108864 -Wl,--strip-all -o "$OUT.tmp" "${objs[@]}" -lunwind
mv "$OUT.tmp" "$OUT"
echo "$OUT: $(wc -c <"$OUT" | tr -d ' ') bytes, sha256 $(sha256 "$OUT")"
