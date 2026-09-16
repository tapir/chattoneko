// Regenerate all Android launcher icon PNGs from the canonical SVGs:
//   icons/app-icon-bg.svg       — full-bleed adaptive background
//   icons/app-icon-fg.svg       — cat face, transparent margins (adaptive safe zone)
//   icons/app-icon-combined.svg — single-image icon for launchers without adaptive support
//
// Adaptive layers render on the 108dp canvas, not at launcher sizes, so the
// launch zoom-in stays sharp (xxxhdpi = 432px); the combined icon renders at
// launcher sizes (48dp).
//
// Requires: rsvg-convert on PATH.
import { execFileSync } from "node:child_process";
import { cpSync } from "node:fs";
import { join } from "node:path";

const iconsDir = join(import.meta.dirname, "..", "icons");
const resDir = join(iconsDir, "..", "android", "app", "src", "main", "res");
const BG = join(iconsDir, "app-icon-bg.svg");
const FG = join(iconsDir, "app-icon-fg.svg");
const COMBINED = join(iconsDir, "app-icon-combined.svg");

const DENSITY = { mdpi: 1, hdpi: 1.5, xhdpi: 2, xxhdpi: 3, xxxhdpi: 4 };

const render = (svg, px, out) =>
  execFileSync("rsvg-convert", ["-w", String(px), "-h", String(px), svg, "-o", out]);

for (const [density, s] of Object.entries(DENSITY)) {
  const dir = join(resDir, `mipmap-${density}`);
  render(BG, 108 * s, join(dir, "ic_launcher_background.png"));
  render(FG, 108 * s, join(dir, "ic_launcher_foreground.png"));
  const legacy = join(dir, "ic_launcher.png");
  render(COMBINED, 48 * s, legacy);
  cpSync(legacy, join(dir, "ic_launcher_round.png")); // identical bytes, no second render
}
console.log("✓ icons regenerated from app-icon-{bg,fg,combined}.svg");
