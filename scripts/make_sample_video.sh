#!/usr/bin/env bash
# Create the videos the project is tested with.
#
#   scripts/make_sample_video.sh sample
#       Rebuild docs/samples/power_jump.mp4 from its public-domain source on
#       Wikimedia Commons (see docs/samples/README.md for the licence).
#
#   scripts/make_sample_video.sh big [OUT] [BYTES]
#       Generate a large, real, decodable video for the resumable-upload test
#       by looping the sample clip into a DNxHR HQ file, the kind of
#       mezzanine file an editor exports. Default: 2.1 GiB at
#       tmp/big_2.1GiB.mov. The content is a person moving, so the worker can
#       process the result end to end as well.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SAMPLE="$ROOT/docs/samples/power_jump.mp4"

SOURCE_URL="https://upload.wikimedia.org/wikipedia/commons/1/17/Conditioning_Drill_1-_Power_Jump.webm"
SOURCE_SHA256="013893481ec1d78068fb05c9b44c858924f282a97d82e8be937046a2dc4f97f1"

sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }

case "${1:-}" in
sample)
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  echo "downloading source clip from Wikimedia Commons"
  # Wikimedia asks for a descriptive User-Agent on automated requests.
  curl -fsSL -A "rigforge-sample/1.0 (https://github.com/ArianKhademi/rigforge)" -o "$tmp/source.webm" "$SOURCE_URL"
  actual="$(sha256 "$tmp/source.webm")"
  if [ "$actual" != "$SOURCE_SHA256" ]; then
    echo "checksum mismatch: the source file changed (got $actual)" >&2
    exit 1
  fi
  mkdir -p "$(dirname "$SAMPLE")"
  # -ss before -i seeks, and because the video is re-encoded the cut is frame
  # accurate. crop takes the centre 1080x1080 of the 1920x1080 frame.
  ffmpeg -hide_banner -loglevel error -y -ss 45.25 -i "$tmp/source.webm" -t 5.2 \
    -vf "crop=1080:1080:420:0,scale=720:720" -an \
    -c:v libx264 -crf 20 -preset slow -pix_fmt yuv420p -movflags +faststart "$SAMPLE"
  echo "wrote $SAMPLE ($(wc -c <"$SAMPLE" | tr -d ' ') bytes)"
  ;;

big)
  out="${2:-$ROOT/tmp/big_2.1GiB.mov}"
  bytes="${3:-2254857830}" # 2.1 GiB
  mkdir -p "$(dirname "$out")"
  echo "generating $out (target $bytes bytes)"
  # -stream_loop -1 repeats the sample forever; -fs stops writing once the
  # file reaches the size limit and finalises the container, so the result is
  # a complete, playable file of (very nearly) the requested size.
  # DNxHR HQ is a constant-bitrate intra-frame codec (about 220 Mbit/s at
  # 1080p30), so the size grows linearly with duration and it encodes far
  # faster than real time. The square sample is padded to 1920x1080.
  ffmpeg -hide_banner -loglevel error -y -stream_loop -1 -i "$SAMPLE" \
    -vf "scale=1080:1080,pad=1920:1080:420:0:white" -an \
    -c:v dnxhd -profile:v dnxhr_hq -pix_fmt yuv422p \
    -fs "$bytes" "$out"
  size="$(wc -c <"$out" | tr -d ' ')"
  duration="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$out")"
  gib="$(awk -v s="$size" 'BEGIN { printf "%.2f", s / 1073741824 }')"
  echo "wrote $out: $size bytes ($gib GiB), ${duration}s of video"
  ;;

*)
  sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
  exit 2
  ;;
esac
