#!/usr/bin/env bash
#
# transcode.sh — build browser-friendly copies of a (read-only) media library
# into a separate writable DERIVED_DIR, so mediavault can serve them for
# formats a browser can't play/show natively.
#
# It is deliberate about cost:
#   * Files already playable in a browser (mp4/webm with H.264/VP9/AV1 + AAC/etc)
#     are SKIPPED — mediavault serves the original.
#   * Files whose streams are web-safe but sit in a non-web container (e.g. an
#     .mkv/.mov/.avi holding H.264 + AAC) are REMUXED with `-c copy` — fast and
#     lossless, no re-encode.
#   * Only files with non-web codecs (HEVC/H.265, DivX/Xvid, VC-1, ...) are
#     RE-ENCODED to H.264/AAC.
#   * Non-web still images (HEIC, TIFF, JP2, RAW, ...) get a downscaled JPEG
#     preview.
#
# Outputs mirror the source tree by relative path, with the original name kept
# and a new extension appended, e.g.  clips/holiday.avi -> clips/holiday.avi.mp4
# That is exactly what mediavault looks for when DERIVED_DIR is set.
#
# Safe to re-run: up-to-date outputs are skipped. Nothing is ever written to the
# source tree.
#
# Usage:
#   ./transcode.sh [SRC] [DST]
#   SRC defaults to $MEDIA_ROOT or /media ; DST to $DERIVED_DIR or /derived
#
# Tunables (env): CRF (default 20), PRESET (veryfast), IMG_MAXPX (2000)
#
set -euo pipefail

SRC="${1:-${MEDIA_ROOT:-/media}}"
DST="${2:-${DERIVED_DIR:-/derived}}"
SRC="${SRC%/}"
DST="${DST%/}"
CRF="${CRF:-20}"
PRESET="${PRESET:-veryfast}"
IMG_MAXPX="${IMG_MAXPX:-2000}"

command -v ffprobe >/dev/null 2>&1 || { echo "error: ffprobe not found on PATH" >&2; exit 1; }
command -v ffmpeg  >/dev/null 2>&1 || { echo "error: ffmpeg not found on PATH"  >&2; exit 1; }
[ -d "$SRC" ] || { echo "error: source '$SRC' is not a directory" >&2; exit 1; }
mkdir -p "$DST"

# Codecs a browser can decode inside mp4/webm.
web_vcodec_re='^(h264|vp8|vp9|av1)$'
web_acodec_re='^(aac|mp3|opus|vorbis|flac)$'
# Audio codecs that are valid *inside mp4* (others get converted to AAC on remux).
mp4_acodec_re='^(aac|mp3|opus)$'

n_skip=0 n_remux=0 n_encode=0 n_img=0 n_fail=0

probe1() { # $1 file, $2 stream selector (v:0 / a:0) -> codec_name or empty
  ffprobe -v error -select_streams "$2" -show_entries stream=codec_name \
          -of default=nw=1:nk=1 "$1" 2>/dev/null | head -n1 || true
}

up_to_date() { # $1 src $2 out -> 0 if out exists and is newer-or-equal to src
  [ -f "$2" ] && [ ! "$1" -nt "$2" ]
}

do_video() { # $1 abs  $2 rel  $3 ext(lowercase)
  local src="$1" rel="$2" ext="$3" out="$DST/$2.mp4"
  local v a
  v="$(probe1 "$src" v:0)"
  a="$(probe1 "$src" a:0)"

  # Already browser-playable? Leave it; mediavault serves the original.
  if [[ "$ext" =~ ^(mp4|m4v|webm)$ ]] && [[ "$v" =~ $web_vcodec_re ]] \
     && { [ -z "$a" ] || [[ "$a" =~ $web_acodec_re ]]; }; then
    n_skip=$((n_skip+1)); return
  fi

  if up_to_date "$src" "$out"; then n_skip=$((n_skip+1)); return; fi
  mkdir -p "$(dirname "$out")"
  local tmp="$out.part"

  if [[ "$v" =~ $web_vcodec_re ]] && { [ -z "$a" ] || [[ "$a" =~ $web_acodec_re ]]; }; then
    # Streams are web-safe: rewrap into mp4 (no video re-encode).
    if [ -n "$a" ] && ! [[ "$a" =~ $mp4_acodec_re ]]; then
      echo "remux+aac: $rel  (v=$v a=$a)"
      ffmpeg -v error -y -i "$src" -c:v copy -c:a aac -b:a 160k -movflags +faststart "$tmp.mp4"
    else
      echo "remux:     $rel  (v=$v a=${a:-none})"
      ffmpeg -v error -y -i "$src" -map 0:v:0 -map 0:a:0? -c copy -movflags +faststart "$tmp.mp4"
    fi
    n_remux=$((n_remux+1))
  else
    echo "encode:    $rel  (v=${v:-?} a=${a:-none})"
    ffmpeg -v error -y -i "$src" \
      -map 0:v:0 -map 0:a:0? \
      -c:v libx264 -preset "$PRESET" -crf "$CRF" -pix_fmt yuv420p \
      -c:a aac -b:a 160k -movflags +faststart "$tmp.mp4"
    n_encode=$((n_encode+1))
  fi
  mv -f "$tmp.mp4" "$out"
}

do_image() { # $1 abs  $2 rel
  local src="$1" rel="$2" out="$DST/$2.jpg"
  if up_to_date "$src" "$out"; then n_skip=$((n_skip+1)); return; fi
  mkdir -p "$(dirname "$out")"
  local tmp="$out.part.jpg"
  if ffmpeg -v error -y -i "$src" -frames:v 1 \
       -vf "scale='min(${IMG_MAXPX},iw)':-1:flags=lanczos" -q:v 3 "$tmp" 2>/dev/null; then
    mv -f "$tmp" "$out"; echo "preview:   $rel"; n_img=$((n_img+1))
  else
    rm -f "$tmp"; echo "no preview (unsupported by ffmpeg): $rel" >&2; n_fail=$((n_fail+1))
  fi
}

while IFS= read -r -d '' f; do
  rel="${f#"$SRC"/}"
  ext="${f##*.}"; ext="$(printf '%s' "$ext" | tr '[:upper:]' '[:lower:]')"
  case "$ext" in
    mp4|m4v|webm|mkv|mov|avi|mpg|mpeg|m2v|ts|m2ts|mts|flv|f4v|vob|wmv|asf|ogv|3gp|3g2|divx)
      do_video "$f" "$rel" "$ext" ;;
    # non-web stills that need a JPEG preview (web-native jpg/png/gif/webp/avif/svg are left alone)
    heic|heif|tif|tiff|jp2|j2k|jpf|tga|exr|hdr|dds|ppm|pgm|pbm|pnm|bmp|jxl|cr2|cr3|nef|arw|dng|raf|orf|rw2|pef)
      do_image "$f" "$rel" ;;
    *) : ;;
  esac
done < <(find "$SRC" -type f -print0)

echo
echo "done: skipped=$n_skip  remuxed=$n_remux  encoded=$n_encode  previews=$n_img  failed=$n_fail"
echo "derived tree: $DST"
