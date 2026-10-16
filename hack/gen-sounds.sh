#!/usr/bin/env bash
# gen-sounds: render the PBX's sound prompts (docs/plans/release-a.md §2(c),
# docs/sounds.md) in every voice the image carries, or in the voices named,
# into <out>/<voice>/ (default build/sounds), each prompt as <name>.ulaw
# (G.711 µ-law, 8 kHz) and <name>.g722 (G.722, 16 kHz). `make sounds` runs
# it inside the flake, which pins espeak-ng and ffmpeg (the dev shell) and
# Piper and its voices (the flake's piper and piper-voices packages, which
# this script builds with `nix build`, so `nix develop` never fetches them;
# or set PIPER to a piper binary and PIPER_VOICES to a directory of
# en_US-<voice>-medium.onnx and .onnx.json).
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
#   ljspeech, kristin, norman, john
#           Piper's en_US-<voice>-medium. Piper's GPL-3.0 covers the
#           program, which runs only here. Each voice was trained on a
#           public domain dataset (LJ Speech; LibriVox recordings), from
#           scratch or, john, fine-tuned from kristin. Never lessac or a
#           voice fine-tuned from it: the lessac dataset's licence allows
#           research use only.
voices=(espeak ljspeech kristin norman john)

# Piper's voices draw random noise on every run (onnxruntime's random ops,
# unseeded; piper 1.8 has no seed option), so one text would render a
# little differently each time. With both noise scales at 0 the render is
# a function of the text and the model: two runs give the same bytes.
# Piper normalises each sentence to full scale; 0.7 of that puts its peaks
# near -3 dBFS, like espeak's, with headroom for the resampler. And 0.2 s
# between sentences, which Piper otherwise runs together.
piper_args=(--noise-scale 0 --noise-w-scale 0 --volume 0.7 --sentence-silence 0.2)

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

# Piper and its voices, from the flake unless given, if any voice needs it.
piper_voices=()
for v in "${want[@]}"; do
	[[ $v == espeak ]] || piper_voices+=("$v")
done
if ((${#piper_voices[@]} > 0)); then
	if [[ -z ${PIPER:-} || -z ${PIPER_VOICES:-} ]]; then
		command -v nix >/dev/null ||
			die "nix is not on PATH: Piper and its voices are the flake's piper and piper-voices packages (or set PIPER and PIPER_VOICES)"
		flake=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
	fi
	if [[ -z ${PIPER:-} ]]; then
		PIPER=$(nix build --no-link --print-out-paths "$flake#piper")/bin/piper
	fi
	if [[ -z ${PIPER_VOICES:-} ]]; then
		PIPER_VOICES=$(nix build --no-link --print-out-paths "$flake#piper-voices")
	fi
	[[ -x $PIPER ]] || die "PIPER=$PIPER is not an executable"
	for v in "${piper_voices[@]}"; do
		for f in "$PIPER_VOICES/en_US-$v-medium.onnx" "$PIPER_VOICES/en_US-$v-medium.onnx.json"; do
			[[ -f $f ]] || die "voice $v: no $f (the flake's piper-voices must carry every Piper voice)"
		done
	done
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render <voice> <name> <dir>: one prompt in one voice, both formats, into dir.
render() {
	local voice=$1 name=$2 dir=$3
	local wav=$work/$voice-$name.wav
	case $voice in
	espeak) espeak-ng -v en-us -s 150 -w "$wav" "${texts[$name]}" ;;
	*)
		printf '%s\n' "${texts[$name]}" |
			"$PIPER" --model "$PIPER_VOICES/en_US-$voice-medium.onnx" "${piper_args[@]}" --output-file "$wav"
		;;
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
