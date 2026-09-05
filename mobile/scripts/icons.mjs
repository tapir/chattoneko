#!/usr/bin/env node
// Regenerate all Android launcher icon PNGs from the canonical SVGs:
//   icons/app-icon-bg.svg       — full-bleed background
//   icons/app-icon-fg.svg       — cat face, transparent margins (safe zone)
//   icons/app-icon-combined.svg — single-image icon (legacy / older launchers)
//
// Adaptive-icon layers (API 26+) are rendered at 108dp per density — NOT
// launcher sizes. A 192px max foreground is what made the launch zoom-in
// blurry; xxxhdpi needs 432px. Legacy icons get the combined SVG at
// launcher sizes.
//
// Requires: rsvg-convert.
import { execFileSync } from "node:child_process";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const iconsDir = resolve(dirname(fileURLToPath(import.meta.url)), "..", "icons");
const resDir = resolve(iconsDir, "..", "android", "app", "src", "main", "res");
const BG = join(iconsDir, "app-icon-bg.svg");
const FG = join(iconsDir, "app-icon-fg.svg");
const COMBINED = join(iconsDir, "app-icon-combined.svg");

// 108dp per density bucket (adaptive icon layers).
const LAYER = { mdpi: 108, hdpi: 162, xhdpi: 216, xxhdpi: 324, xxxhdpi: 432 };
// Launcher glyph sizes (legacy, non-adaptive fallback).
const LEGACY = { mdpi: 48, hdpi: 72, xhdpi: 96, xxhdpi: 144, xxxhdpi: 192 };

const render = (svg, px, out) =>
  execFileSync("rsvg-convert", ["-w", String(px), "-h", String(px), svg, "-o", out]);

for (const density of Object.keys(LAYER)) {
  const dir = join(resDir, `mipmap-${density}`);
  render(BG, LAYER[density], join(dir, "ic_launcher_background.png"));
  render(FG, LAYER[density], join(dir, "ic_launcher_foreground.png"));
  render(COMBINED, LEGACY[density], join(dir, "ic_launcher.png"));
  render(COMBINED, LEGACY[density], join(dir, "ic_launcher_round.png"));
  console.log(`${density}: bg/fg ${LAYER[density]}px, legacy ${LEGACY[density]}px`);
}
console.log("✓ icons regenerated from app-icon-{bg,fg,combined}.svg");
