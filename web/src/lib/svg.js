// SVG attachments: which ones preview as a picture, and the source they paint
// from.
//
// The server stores an SVG under the text kind and serves it text/plain
// (internal/attach), so it can never execute in the app's origin. The previews
// keep that property: the source is fetched as text and handed to an <img> as
// a blob URL — the lightbox stage and the chat thumbnails both — and an image
// context is the browser's secure static mode: no scripting, no external
// references, while CSS and SMIL animation still run.

export const isSvg = (att) =>
  att?.kind === 'text' && /\.svg$/i.test((att.filename ?? '').trim());

// The source rewritten with intrinsic width/height, or null when `text` is not
// an SVG document at all — a misnamed file, or malformed XML, which the parser
// answers with a parsererror root and no SVG namespace.
//
// The injection is what makes an SVG size like a raster: a viewBox-only file
// has an intrinsic RATIO but no intrinsic SIZE, so an auto-sized <img> falls
// to the browser's 300×150 default, and writing a pixel size inline is no way
// out either — max-* constraints re-solve a ratio only while both sides are
// auto, so a written pair lets max-height squash the box under the art and
// preserveAspectRatio letterboxes the leftover as side bands. With intrinsic
// dimensions the plain max-h/max-w contain every thumbnail already uses does
// the whole job.
//
// The size written is the viewBox ratio at the width/height attributes'
// magnitude: the art PAINTS at the viewBox ratio (preserveAspectRatio
// letterboxes any other box), so attributes that disagree with it would band
// the picture even at natural size.
export function svgSource(text) {
  let doc;
  try {
    doc = new DOMParser().parseFromString(text ?? '', 'image/svg+xml');
  } catch {
    return null;
  }
  const root = doc.documentElement;
  // Exactly `svg` in exactly the SVG namespace: that is what the <img>
  // decoder requires, and both halves fail on a parse error.
  if (root?.namespaceURI !== 'http://www.w3.org/2000/svg' || root.localName !== 'svg') {
    return null;
  }
  // Only a bare number (or px) is a length here: "100%" and "2em" size against
  // a viewport the <img> does not have, so they fall through to the viewBox
  // like a missing attribute does.
  const px = (v) => (/^\d*\.?\d+(px)?$/i.test(v?.trim() ?? '') ? parseFloat(v) : 0);
  const vb = (root.getAttribute('viewBox') ?? '').trim().split(/[\s,]+/).map(Number);
  const box =
    vb.length === 4 && vb.slice(2).every((n) => Number.isFinite(n) && n > 0)
      ? { w: vb[2], h: vb[3] }
      : null;
  let w = px(root.getAttribute('width')) || box?.w || 0;
  let h = px(root.getAttribute('height')) || box?.h || 0;
  if (box?.w && box?.h) {
    if (w) h = (w * box.h) / box.w;
    else if (h) w = (h * box.w) / box.h;
  }
  // Neither dimension nor a viewBox: nothing to inject, and the browser's own
  // default sizing is the file's normal look anyway.
  if (w && h) {
    root.setAttribute('width', String(w));
    root.setAttribute('height', String(h));
  }
  return new XMLSerializer().serializeToString(doc);
}
