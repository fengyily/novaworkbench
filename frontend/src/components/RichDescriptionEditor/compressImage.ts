// compressImage — client-side image compressor for the RichDescriptionEditor.
//
// Goal: shrink user-pasted screenshots / dumps to a size Claude can comfortably
// Read (longest side ≤ maxDim, target ≤ maxBytes). Runs entirely in the
// browser via the standard OffscreenCanvas / createImageBitmap pipeline so we
// don't add any npm dependency, and the only network cost is the (already
// smaller) upload that follows.
//
// Failure mode: anything we can't decode or shrink falls through to the
// original file. The backend's AttachmentService will still accept it; a
// 50 MB raw PNG just costs more upload bandwidth and tokens on the LLM side,
// but we never block the user from submitting.

export interface CompressImageOptions {
  /** Longest-edge cap in pixels. Images smaller than this are passed through. */
  maxDim?: number;
  /** Starting JPEG quality (0–1). Lowered in 0.1 steps when the blob still
   *  exceeds maxBytes; floored at 0.5. Ignored for PNG. */
  jpegQuality?: number;
  /** Target upper bound. The compressor lowers quality (down to 0.5) until
   *  the output fits, then gives up and returns whatever it has. */
  maxBytes?: number;
  /** Files larger than this are passed through untouched — protects against
   *  4K-phone-photo-style blowups where a canvas re-decode would OOM the tab. */
  skipThresholdBytes?: number;
}

const DEFAULTS: Required<CompressImageOptions> = {
  maxDim: 1600,
  jpegQuality: 0.85,
  maxBytes: 2 * 1024 * 1024,
  skipThresholdBytes: 50 * 1024 * 1024,
};

/** Output MIME: PNG stays PNG (lossless), everything else becomes JPEG. */
function pickOutputMime(inputType: string): string {
  return inputType === 'image/png' ? 'image/png' : 'image/jpeg';
}

/** Lowest quality we'll try before giving up on the size cap. */
const MIN_JPEG_QUALITY = 0.5;
const QUALITY_STEP = 0.1;
const MAX_ITERATIONS = 5;

/**
 * Compress an image file in the browser. Returns the original blob on any
 * failure (unsupported format, decode error, oversize short-circuit) so
 * callers can chain straight into the upload without a try/catch.
 */
export async function compressImage(
  file: File | Blob,
  opts?: CompressImageOptions,
): Promise<Blob> {
  const o = { ...DEFAULTS, ...opts };

  // Short-circuit: too big, not an image, or animated GIF (we don't re-encode
  // animation since OffscreenCanvas only decodes the first frame).
  if (file.size > o.skipThresholdBytes) return file;
  const mime = (file as File).type || (file as Blob & { type?: string }).type || '';
  if (!mime.startsWith('image/')) return file;
  if (mime === 'image/gif') return file;

  let bitmap: ImageBitmap;
  try {
    bitmap = await createImageBitmap(file);
  } catch {
    return file;
  }

  try {
    const longest = Math.max(bitmap.width, bitmap.height);
    const scale = longest > o.maxDim ? o.maxDim / longest : 1;
    const w = Math.max(1, Math.round(bitmap.width * scale));
    const h = Math.max(1, Math.round(bitmap.height * scale));

    const canvas = new OffscreenCanvas(w, h);
    const ctx = canvas.getContext('2d');
    if (!ctx) return file;
    ctx.drawImage(bitmap, 0, 0, w, h);

    const outMime = pickOutputMime(mime);
    const isJpegLike = outMime === 'image/jpeg' || outMime === 'image/webp';
    let quality = o.jpegQuality;
    let blob: Blob;

    // JPEG/WebP: iterate quality until the blob fits under maxBytes (or we
    // hit the floor). PNG has no quality knob, so emit once and ship.
    if (isJpegLike) {
      blob = await canvas.convertToBlob({ type: outMime, quality });
      let iter = 0;
      while (blob.size > o.maxBytes && quality > MIN_JPEG_QUALITY && iter < MAX_ITERATIONS) {
        quality = Math.max(MIN_JPEG_QUALITY, quality - QUALITY_STEP);
        blob = await canvas.convertToBlob({ type: outMime, quality });
        iter += 1;
      }
    } else {
      blob = await canvas.convertToBlob({ type: outMime });
    }

    // If our "compressed" output is actually larger than the input (rare but
    // possible for tiny PNGs), keep the input.
    return blob.size < file.size ? blob : file;
  } finally {
    bitmap.close();
  }
}
