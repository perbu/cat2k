# Cat Tracker — Control Panel Design Spec

Handoff spec for the redesigned left control panel. The map (Leaflet/OSM + heatmap layer)
is unchanged; this covers the panel only.

## Design intent

The panel grew organically and had redundancy (a "Trackers" list separate from "Activity"
charts) and clutter (raw heat-map sliders exposed). The redesign:

1. **One card per cat** merges identity + status + activity: color dot, name, last-seen,
   battery, weekly distance chart, today / weekly totals.
2. **Heat map auto-tunes.** A single "Auto-tune" toggle (default ON). The Intensity /
   Radius / Blur sliders stay available but are tucked below and disabled while auto is on.
3. **Statistics folded away** — position count + updated time live in the header caption
   and a slim footer instead of their own block.
4. **One accent system:** charcoal (`#16181D`) for actions/UI; each cat's color is used
   only for that cat's own dot + chart. Numeric readouts use a monospace face.

---

## Design tokens

### Color

| Role | Hex |
|---|---|
| Ink / primary text / buttons | `#16181D` |
| Text secondary | `#5B626C` |
| Text tertiary / labels | `#9198A2` |
| Text faint (day labels) | `#B4BAC2` |
| Panel surface | `#FFFFFF` |
| Card surface | `#F8F9FA` |
| Card border | `#ECEEF1` |
| Hairline / dividers | `#EEF0F2` |
| Segmented-control track | `#F0F2F4` |
| Slider track | `#DFE3E7` |
| Page canvas (behind panel) | `#E7EBEA` |
| **Bella** (accent) | `#17BE7E` — chart bars muted `#BEEBD6`, halo `rgba(23,190,126,0.16)` |
| **Luna** (accent) | `#F5991F` — chart bars muted `#FBDDB4`, halo `rgba(245,153,31,0.16)` |
| Battery fill (normal) | `#7A828C`; shell border `#A8AEB6` |
| Battery low (<20%) | `#E08A00` (fill + text) — *rule, not yet triggered in mock* |

### Type

- **UI:** `Hanken Grotesk` (400/500/600/700).
- **Numerals** (km, %, counts, timestamps): `JetBrains Mono` (400/500/600). Use for any
  figure that benefits from tabular alignment.
- Section labels: 11px, weight 600, `text-transform:uppercase`, `letter-spacing:0.08em`,
  color tertiary.
- Card title (cat name): 14.5px / 700 / `letter-spacing:-0.01em`.
- Body meta: 12px, tertiary. Captions: 11.5px.

### Spacing & shape

- Panel width **344px**, full height, padding **20px**, `gap:18px` between sections.
- Cards: radius **14px**, padding **13px 14px**, `gap` 10–12px internally.
- Buttons / segmented items: radius 10–11px. Pills/toggles: radius 20px.
- Bars: radius 3px. Slider thumb: 12px circle, white fill, 1.5px ink border.

---

## Components

### Header
Row: brand mark (26px rounded-square, ink bg, white paw glyph) + "Cat Tracker" (16/700),
right-aligned **Live** indicator (uppercase 11px, Bella-green, pulsing dot via
`@keyframes` scaling 1→2.4 + fade, 2.4s infinite).

### Update
Full-width ink button (40px, radius 11) with refresh glyph + label. Caption row below:
`Updated 6:48 PM` (left) · `3,526 positions` (right, mono).

### Date range
Segmented control: track `#F0F2F4`, 3px padding. Active segment = white fill + subtle
shadow + ink text; inactive = secondary text. Options: `7d / 30d / 90d / Custom`.

### Cat card (repeated per cat — always exactly 2: Bella, Luna)
```
[● name]                    [battery pill  84%]
Last seen 2 min ago · nr. Lillevann
▁▃▂▃█▁▄   ← 7 bars, height=%, last bar = accent (today), rest = muted accent
Mon Tue Wed Thu Fri Sat Today(accent,bold)
────────────────────────────────
3.6 km today                     22.4 km / wk
```
- Battery pill: 18×10 shell (1.4px border) + nub, inner fill width = charge %, then `%` in mono.
- Chart: `display:flex; align-items:flex-end; height:52px; gap:5px`; bar heights as %.

### Heat map card
- "Auto-tune" title + subtext, right-aligned pill toggle (ink when ON, knob 18px right).
- Below: Intensity / Radius / Blur rows (label 56px · track+fill+thumb · mono value),
  wrapper `opacity:0.42; pointer-events:none` while auto is ON.
- **When auto OFF:** enable the slider group; values feed the heatmap layer
  (`intensity` 0–1, `radius` px, `blur` px). Auto ON = derive these from zoom/point density.

### Footer
Top hairline, `3,526 positions` (mono) left · `since Jul 5, 2026` right, tertiary.

---

## Implementation notes

- Mock uses static data. Live values to wire: per-cat `lastSeen`, `place`, `battery`,
  `today`/`week` km + 7-day bar array; global `positionCount`, `lastUpdate`, `dateRange`.
- Battery: apply low-color rule below 20%; consider mid-tint 20–50%.
- Only two trackers exist by design — no need to make the card list dynamic-length.
- Keep the accent-per-cat rule strict: never use green/orange for chrome, never charcoal
  for a cat.

