#!/usr/bin/env bash
# gen-sounds: render the PBX's sound prompts (docs/plans/release-a.md §2(c),
# docs/sounds.md) in every voice the image carries, or in the voices named,
# into <out>/<voice>/ (default build/sounds), each prompt as <name>.ulaw
# (G.711 µ-law, 8 kHz) and <name>.g722 (G.722, 16 kHz). `make sounds` runs
# it inside the flake, which pins espeak-ng and ffmpeg.
#
#   hack/gen-sounds.sh [<out> [<voice>...]]
#   hack/gen-sounds.sh --list       the voices, one per line
#
# Text-to-speech from the texts below, so no third-party audio is needed and
# nothing binary is committed: the output is a build product (.gitignore has
# /build/), copied into the image by the Dockerfile, and the agent installs
# one voice (--sounds-voice). Two runs with the same flake give
# byte-identical files; the script checks that by rendering each voice
# twice.
#
# The names must match compile.SoundNoEmergency and compile.SoundNotAvailable
# (the dialplan plays <SoundsDir>/<name>), the voices internal/agent/sounds'
# Voices (its tests run --list), and internal/agent/sounds checks that every
# prompt is installed before the agent reports ready.
set -euo pipefail

# The voices: an allowlist. Nothing else is rendered or accepted, and the
# agent holds the same list (sounds.Voices). Each one's licence lets a
# public image carry what it says (docs/sounds.md):
#   espeak  espeak-ng's own en-us voice, never an mbrola mb-* voice (whose
#           data has a licence of its own). espeak-ng's GPL-3.0 covers the
#           program, not its output.
voices=(espeak)

declare -A texts=(
	[no-emergency]="This phone cannot call emergency services. Hang up and use a mobile phone."
	[not-available]="That number is not available."
)

die() {
	echo "gen-sounds: $*" >&2
	exit 1
}

known() {
	local v
	for v in "${voices[@]}"; do
		[[ $1 == "$v" ]] && return 0
	done
	return 1
}

if [[ ${1:-} == --list ]]; then
	printf '%s\n' "${voices[@]}"
	exit 0
fi
out=${1:-build/sounds}
(($# == 0)) || shift
want=("$@")
((${#want[@]} > 0)) || want=("${voices[@]}")
for v in "${want[@]}"; do
	known "$v" || die "unknown voice \"$v\"; the voices are: ${voices[*]}"
done

for tool in espeak-ng ffmpeg; do
	command -v "$tool" >/dev/null || die "$tool is not on PATH: run inside 'nix develop'"
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render <voice> <name> <dir>: one prompt in one voice, both formats, into dir.
render() {
	local voice=$1 name=$2 dir=$3
	local wav=$work/$voice-$name.wav
	case $voice in
	espeak) espeak-ng -v en-us -s 150 -w "$wav" "${texts[$name]}" ;;
	*) die "no renderer for voice $voice" ;;
	esac
	ffmpeg -hide_banner -loglevel error -nostdin -y -i "$wav" \
		-ar 8000 -ac 1 -acodec pcm_mulaw -f mulaw "$dir/$name.ulaw"
	ffmpeg -hide_banner -loglevel error -nostdin -y -i "$wav" \
		-ar 16000 -ac 1 -acodec g722 -f g722 "$dir/$name.g722"
}

for voice in "${want[@]}"; do
	mkdir -p "$work/a/$voice" "$work/b/$voice"
	for name in "${!texts[@]}"; do
		render "$voice" "$name" "$work/a/$voice"
		render "$voice" "$name" "$work/b/$voice"
	done
	if ! diff -r "$work/a/$voice" "$work/b/$voice" >/dev/null; then
		die "two renders in the $voice voice differ; the prompts are not reproducible"
	fi
done

mkdir -p "$out"
# Before there were voices the prompts lay in <out> itself; the image
# copies all of <out>, so none may be left there.
for name in "${!texts[@]}"; do
	rm -f "$out/$name.ulaw" "$out/$name.g722"
done
for voice in "${want[@]}"; do
	mkdir -p "$out/$voice"
	for f in "$work/a/$voice"/*; do
		install -m 0644 "$f" "$out/$voice/"
	done
	# Both formats run at 8000 bytes a second (G.711 µ-law: 8 kHz, 8 bits;
	# G.722: 64 kbit/s).
	for f in "$out/$voice"/*; do
		size=$(stat -c %s "$f")
		printf 'gen-sounds: %s (%s bytes, %s s)\n' "$f" "$size" "$(awk -v b="$size" 'BEGIN { printf "%.2f", b / 8000 }')"
	done
done
