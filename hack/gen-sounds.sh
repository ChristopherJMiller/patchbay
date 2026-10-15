#!/usr/bin/env bash
# gen-sounds: render the PBX's sound prompts (docs/plans/release-a.md §2(c))
# into <out> (default build/sounds), each as <name>.ulaw (G.711 µ-law, 8 kHz)
# and <name>.g722 (G.722, 16 kHz). `make sounds` runs it inside the flake,
# which pins espeak-ng and ffmpeg.
#
# Text-to-speech from the texts below, with espeak-ng's own en-us voice
# (never an mbrola mb-* voice, whose data has a licence of its own), so no
# third-party audio is needed and nothing binary is committed: the output
# is a build product (.gitignore has /build/), copied into the image by the
# Dockerfile. Two runs with the same flake give byte-identical files; the
# script checks that by rendering twice.
#
# The names must match compile.SoundNoEmergency and compile.SoundNotAvailable
# (the dialplan plays <SoundsDir>/<name>), and internal/agent/sounds checks
# that every one is installed before the agent reports ready.
set -euo pipefail

out=${1:-build/sounds}

declare -A texts=(
	[no-emergency]="This phone cannot call emergency services. Hang up and use a mobile phone."
	[not-available]="That number is not available."
)

for tool in espeak-ng ffmpeg; do
	command -v "$tool" >/dev/null || {
		echo "gen-sounds: $tool is not on PATH: run inside 'nix develop'" >&2
		exit 1
	}
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# render <name> <dir>: one prompt, both formats, into dir.
render() {
	local name=$1 dir=$2
	espeak-ng -v en-us -s 150 -w "$work/$name.wav" "${texts[$name]}"
	ffmpeg -hide_banner -loglevel error -nostdin -y -i "$work/$name.wav" \
		-ar 8000 -ac 1 -acodec pcm_mulaw -f mulaw "$dir/$name.ulaw"
	ffmpeg -hide_banner -loglevel error -nostdin -y -i "$work/$name.wav" \
		-ar 16000 -ac 1 -acodec g722 -f g722 "$dir/$name.g722"
}

mkdir -p "$work/a" "$work/b"
for name in "${!texts[@]}"; do
	render "$name" "$work/a"
	render "$name" "$work/b"
done
if ! diff -r "$work/a" "$work/b" >/dev/null; then
	echo "gen-sounds: two renders differ; the prompts are not reproducible" >&2
	exit 1
fi

mkdir -p "$out"
for f in "$work"/a/*; do
	install -m 0644 "$f" "$out/"
done
for f in "$out"/*; do
	printf 'gen-sounds: %s (%s bytes)\n' "$f" "$(stat -c %s "$f")"
done
