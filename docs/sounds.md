# Sound prompts and their voice

The PBX speaks two prompts, rendered from text at image build and never
committed (docs/plans/release-a.md §2(c)):

| Prompt | Text | Played for |
|--------|------|-----------|
| `no-emergency` | "This phone cannot call emergency services. Hang up and use a mobile phone." | the emergency number (`911`), three times |
| `not-available` | "That number is not available." | reserved and unassigned numbers |

`make sounds` (`hack/gen-sounds.sh`) renders both in every voice below into
`build/sounds/<voice>/<name>.ulaw` (G.711 µ-law, 8 kHz) and `.g722` (G.722,
16 kHz), each voice twice, and fails unless the two renders are
byte-identical. The image carries all of them under
`/usr/share/patchbay/sounds/<voice>/`; the agent installs the one the chart
picks into the sounds volume under the names the dialplan plays, and is not
ready until every prompt is there.

## Choosing a voice

`sounds.voice` in the chart's values (default `espeak`):

```sh
helm upgrade patchbay charts/patchbay -n telephony --reuse-values --set sounds.voice=kristin
```

It becomes the agent's `--sounds-voice`, in the PBX pod's spec, so a change
restarts the PBX pod (calls in progress drop). Any value not in the table
fails `helm template`, and the agent refuses to start with one, naming the
voices.

To hear them, convert one to WAV (the dev shell's ffmpeg has no player):

```sh
ffmpeg -f mulaw -ar 8000 -i build/sounds/kristin/no-emergency.ulaw /tmp/kristin.wav
```

## The voices and their licences

The repository is public and so is the image, so a voice is allowed only if
what it says may be published: the engine's licence must not reach its
output, and the voice's training data and every model it was fine-tuned
from must be free of use restrictions.

| Voice | Engine | Sounds | Training data | Data licence | Lineage |
|-------|--------|--------|---------------|--------------|---------|
| `espeak` | espeak-ng 1.52, `en-us`, 150 wpm | clear, robotic | none (rule-based formant synthesis) | n/a | espeak-ng's own voice, never an mbrola `mb-*` voice (whose data has its own licence) |
| `ljspeech` | Piper, `en_US-ljspeech-medium` | US English, female | [LJ Speech](https://keithito.com/LJ-Speech-Dataset/) (one reader, LibriVox) | public domain | trained from scratch (Bryce Beattie) |
| `kristin` | Piper, `en_US-kristin-medium` | US English, female | 11.5 h of LibriVox recordings | public domain | trained from scratch (Bryce Beattie) |
| `norman` | Piper, `en_US-norman-medium` | US English, male | 15.5 h of LibriVox recordings | public domain | trained from scratch (Bryce Beattie) |
| `john` | Piper, `en_US-john-medium` | US English, male | 12.5 h of LibriVox recordings | public domain | fine-tuned from `kristin` (Bryce Beattie) |

The data and lineage columns are each voice's model card in
[rhasspy/piper-voices](https://huggingface.co/rhasspy/piper-voices) (MIT), at
the commit `flake.nix` pins.

**The engines** run only at build time, and only their output reaches the
image:

| Engine | Licence | Where |
|--------|---------|-------|
| espeak-ng | GPL-3.0-or-later | the dev shell (`nix develop`) |
| Piper (piper-tts 1.8.0, OHF-Voice/piper1-gpl) | GPL-3.0-only | the flake's `piper` package, built by `gen-sounds` with `nix build .#piper` |

The GPL covers the programs, not the audio they produce, which is not a
derivative of the program. Neither program nor any voice model is in the
image: only the rendered `.ulaw` and `.g722` files.

**Why lessac and its descendants are excluded.** Piper's best-known US
voice, `en_US-lessac`, is trained on the Blizzard Challenge 2013 Lessac
data, which Voice Factory and Lessac Technologies license under a *Research
Licence Agreement*: research purposes only, explicitly not "voice synthesis
products or services", non-transferable and without the right to
sub-license. Audio from that voice in a public image is outside it. Many of
Piper's English voices are fine-tuned from lessac and carry the same
question whatever their own data: `amy`, `ryan`, `joe` (whose data is CC0)
and `hfc_female`, for example, and `danny` and `kathleen` are fine-tuned
from `ryan`. So the allowlist holds only voices whose whole chain is public
domain.

## Reproducible renders

espeak-ng is deterministic. Piper's VITS models draw random noise in every
run (onnxruntime's random ops, unseeded), and piper 1.8 has no seed option,
so `gen-sounds` runs it with `--noise-scale 0 --noise-w-scale 0`: the render
is then a function of the text and the model, and two runs give the same
bytes (one prompt was also checked under `taskset` at 1, 2 and 16 CPUs).
Each phoneme's length and sound is then the model's mean rather than a
sample; durations and levels came out within 0.6 s and about 1 dB of a
render with the voices' own noise (seeded). Whether that sounds flatter is
a question for the ear. (`onnxruntime.set_seed` before Piper's Python API
also gives identical runs, with the voices' own noise scales, at the cost
of a Python wrapper around piper; not used.)

Piper normalises each sentence to full scale, so `--volume 0.7` puts the
peaks near -3 dBFS, like espeak's, and `--sentence-silence 0.2` keeps the
911 prompt's two sentences apart. The bytes are reproducible on one
machine; another CPU may round differently, which is why nothing pins them.

Piper and its voices are flake packages, not part of the dev shell (Piper's
closure is about 1 GiB): `gen-sounds` builds `.#piper` and `.#piper-voices`
when it renders a Piper voice, or takes `PIPER` (a piper binary) and
`PIPER_VOICES` (a directory of `en_US-<voice>-medium.onnx` and
`.onnx.json`). The voices are fixed-output fetches from rhasspy/piper-voices
at a pinned commit, each file pinned by its sha256, about 60 MB each.

## Adding a voice

Check its model card first: the dataset must be public domain (or CC0) and
nothing it was fine-tuned from may be lessac-based or otherwise restricted.
Then add it, with its row in the table above, to all four lists:

- `internal/agent/sounds` `Voices` (with the reasoning in its comment);
- `hack/gen-sounds.sh` `voices`;
- `flake.nix` `piper-voices` (both files' sha256: `nix store prefetch-file <url>`);
- the chart's `patchbay.soundsVoice` helper and the list in `values.yaml`.

The tests compare the script's `--list` and the chart's refusal message with
`Voices`, and `make sounds` fails if a Piper voice's model is missing from
`piper-voices`.
