i# DESIGN.md — تودج / Toodej

Design system for the Toodej storefront, a family-run online shop selling
fresh figs, pomegranates, and their derived products (like رب انار) straight
from the garden. This document defines the visual language, typography,
color system, layout rules, and component direction for the site. The site
itself is in Persian and reads right-to-left; this document is in English
for the dev team.

## 1. Brand Concept

Toodej is not a generic grocery storefront. It should feel like stepping
into a real orchard that has been in the family for generations: sun on
terracotta walls, the deep stain of pomegranate juice on a wooden table,
fig leaves casting soft shadows. The goal is warmth and provenance over
polish. Nothing should look like a templated Shopify theme or a default AI
design pass — no generic glassmorphism, no purple-to-blue SaaS gradients,
no perfectly symmetric hero sections with stock-photo produce on a white
background.

Guiding principles:

- Handmade over corporate. Small imperfections (slightly rotated cards,
  hand-drawn dividers, torn-paper textures) read as authentic.
- Warm and personal copywriting tone reflected visually — this is a family
  garden, not an agribusiness.
- Seasonal storytelling. The site should visually hint at harvest, drying,
  and preserving — the actual process behind the products.
- Rich, saturated color drawn directly from the produce itself, not from a
  generic brand palette.

## 2. Color Palette

Colors are drawn from the fruit and the orchard, not from a color wheel
exercise. Avoid flat, saturated "app" colors — lean toward the slightly
muted, sun-warmed versions you'd actually see on ripe fruit and old wood.

### Primary

- Pomegranate red — `#9E2A2B` — primary brand color, buttons, price tags,
  key accents
- Fig purple — `#5B3A5C` — secondary accent, used for headings on light
  backgrounds, badges
- Orchard green — `#3F5D42` — used for leaves, success states, nav
  underlines, organic textures

### Neutrals

- Parchment cream — `#F5EEE0` — main background, replaces plain white
- Warm sand — `#EBDFC9` — section backgrounds, card fills
- Aged walnut — `#3A2B24` — primary text color, replaces pure black
- Soft clay — `#8C6F5E` — secondary text, captions, metadata

### Accents (used sparingly)

- Saffron gold — `#C98A2C` — highlights, ratings, "harvest fresh" tags,
  hover states
- Dried rose — `#C97064` — used for rob-e-anar / paste product category tags

### Usage rules

- Backgrounds are never pure white. Use parchment cream or warm sand.
- Text is never pure black. Use aged walnut for body copy.
- Pomegranate red is reserved for primary actions (add to cart, checkout,
  price). Do not use it decoratively everywhere or it loses meaning.
- Gradients are avoided almost entirely. Where a gradient is unavoidable
  (e.g. a hero image overlay for text legibility) it should go from aged
  walnut at low opacity to transparent, never a rainbow or brand-color
  gradient.

## 3. Typography

Two typefaces, each doing one job. Both are open-source and support Farsi
script properly with real Arabic/Persian glyph shaping (not a Latin font
faking it).

### Estedad — body and UI text

Used for paragraphs, navigation, buttons, form labels, product
descriptions, prices, and all functional UI copy. Estedad is a sans-serif
Arabic-Latin variable typeface, optimized for screens, with 9 weights from
Thin to Black. It reads clearly at small sizes, which matters for product
specs and cart tables.

- Body text: Estedad Regular (400)
- UI labels, nav, buttons: Estedad Medium (500) or SemiBold (600)
- Emphasis within body text: Estedad Bold (700)
- Source: `https://github.com/aminabedi68/Estedad` (SIL OFL license, also
  available via Google Fonts / fontiran)

### Alyamama — headings and display

Used for the site logotype, page titles, section headings, and hero
statements. Alyamama is a variable Arabic typeface with a Naskh-inspired
design — it has the warmth of traditional calligraphy without becoming
hard to read, which fits the heritage-orchard feel far better than a
geometric display font would.

- Hero / logo: Alyamama Black or ExtraBold (800–900)
- Section headings (h1, h2): Alyamama Bold (700)
- Sub-headings (h3, product names): Alyamama SemiBold (600)
- Never use Alyamama for paragraphs or long text — it is a display face
- Source: `https://github.com/Mestaratype/AlYamama` (SIL OFL license, also
  on Google Fonts)

### Type scale (base 16px, mobile-first, scales up ~1.125 per step on desktop)

- Display / hero: 40px mobile, 64px desktop, Alyamama
- H1: 32px mobile, 48px desktop, Alyamama
- H2: 24px mobile, 32px desktop, Alyamama
- H3: 20px mobile, 24px desktop, Alyamama
- Body: 16px, Estedad Regular, line-height 1.8 (Persian script needs more
  vertical breathing room than Latin)
- Small / caption: 13px, Estedad Regular, Soft clay color

### Persian-specific typography notes

- Line-height must be generous (1.7–1.9 for body) because Persian
  diacritics and descenders need more room than Latin text.
- Numbers: use Persian digits (۰۱۲۳۴۵۶۷۸۹) for prices and quantities shown
  to the user, not Latin digits. Keep Latin digits only in code/URLs.
- Never justify Persian body text — it breaks word spacing badly. Left
  ragged edge on the visual left (which is the end of the line in RTL).
- Avoid italics; Persian typefaces generally do not have a true italic and
  faux-slanting looks broken. Use color or weight for emphasis instead.

## 4. Layout & Direction

- Full RTL layout. `dir="rtl"` on `<html>`, logical CSS properties
  (`margin-inline-start`, not `margin-left`) throughout so nothing has to
  be manually mirrored later.
- Logo and primary nav sit on the right in the header (RTL reading start).
  Cart icon and account icon sit on the left.
- Grid: 12-column on desktop, 4-column on mobile, generous gutters (24px+)
  — avoid the cramped, dense grid look of typical grocery e-commerce.
- Section spacing is intentionally loose (96–140px between major sections
  on desktop) to give the site a slower, unhurried "orchard" pace rather
  than a high-density marketplace pace.
- Cards and containers use soft, slightly irregular rounded corners
  (e.g. 18px radius with a subtle asymmetry, like a hand-cut label) rather
  than perfect uniform 8px radii on everything.

## 5. Texture, Imagery & Motifs

This is where "not AI-looking" mostly gets won or lost — through texture
and imperfection, not just color choice.

- Background texture: a very subtle paper/linen grain overlay (2–4%
  opacity) on the parchment background, not a flat fill.
- Divider motif: a hand-drawn-style branch or fig-leaf line divider
  between sections instead of a plain `<hr>` or straight rule.
- Product photography: warm, natural light, shallow depth of field, shot
  on wood or linen surfaces — not white-background studio product shots.
  Include some "in the orchard" and "in process" (drying figs, pouring
  رب انار) photography, not just packaged product shots.
- Stamps and labels: category badges styled like a wax seal or a hand
  stamped ink mark (e.g. "برداشت تازه" / "دست‌ساز خانگی") rather than a flat
  rounded-pill badge.
- Icons: use a custom or hand-finished icon set with slightly organic,
  uneven line weights rather than a perfectly uniform Material/Feather
  icon set. If using a base library, add subtle stroke-width variation or
  a custom leaf/branch icon for category markers.
- Avoid: drop shadows that look like generic Bootstrap/Material elevation,
  perfectly centered symmetric hero layouts, stock "farmer holding basket
  smiling at camera" imagery, purple gradient blobs, glassmorphism cards.

## 6. Pages & Key Components

### Home

- Hero: full-bleed seasonal orchard photo with a short Alyamama headline
  overlay (e.g. the family story in one line) and a single primary CTA
  ("مشاهده محصولات"). Overlay uses the walnut-to-transparent gradient rule
  from Section 2, not a color gradient.
- "Story strip" section: 2–3 lines about the family garden, paired with a
  process photo (drying/harvest), styled like a scrapbook note, not a
  corporate "About us" card.
- Featured products: horizontal card row, wax-seal style badges for
  "تازه" (fresh) / "دست‌ساز" (handmade).
- Seasonal banner: rotates by harvest season (fig season vs. pomegranate
  season) — background photo and accent color shift slightly to match.

### Shop / product listing

- Filter sidebar (right-aligned) by category. The category names below are
  illustrative of the *intent*; the live storefront actually uses a
  **seasonal taxonomy** — `بهار` (spring), `تابستان` (summer), `پاییز` (autumn),
  `خشکبار` (dried), `سنتی` (traditional) — surfaced in the header nav and the
  `/products/{season}` routes. Keep any new category copy consistent with that
  seasonal set rather than the generic grocery labels.
- Product cards: photo, name in Alyamama SemiBold, price in Estedad Bold
  with Persian numerals, small handmade-style tag if applicable.
- Grid loosens on hover — card lifts slightly and rotates 0.5–1 degree,
  reinforcing the handmade, non-uniform feel.

### Product detail

- Large image gallery, at least one lifestyle/process shot in addition to
  the product itself.
- Price, weight/size selector, quantity stepper, add-to-cart button in
  pomegranate red.
- "از باغ تا خانه شما" (garden-to-home) mini timeline showing
  harvest → processing → packaging → delivery, as a small illustrated
  horizontal strip, not a plain numbered list.
- Related products styled the same as the shop grid for consistency.

### Cart

- Simple line-item list, Estedad throughout, Persian numerals for prices
  and quantities.
- Order summary card in warm sand background with a hand-drawn-style
  divider before the total.
- Empty-cart state uses a small illustration (empty fruit basket) rather
  than a generic icon.

### Checkout

- Single-column, step-based (address → delivery → payment → review) to
  keep it calm and low-friction, not a dense multi-panel form.
- Form fields have soft rounded corners matching the card radius,
  Estedad Regular for input text, SemiBold for field labels.
- Trust markers (secure payment, return policy) placed near the payment
  step, styled as small stamped icons, not generic badge images.

### About

- Longer-form storytelling page: family history, the garden, how the
  products are made. This is the most "editorial" page — larger imagery,
  pull-quotes in Alyamama, generous whitespace.

## 7. Motion

- Keep motion minimal, slow, and organic: gentle fade/rise on scroll
  (300–400ms, ease-out), subtle card tilt on hover as noted above.
- Avoid bouncy, springy, or fast micro-interactions — they clash with the
  slow, warm brand feel.

## 8. Accessibility

- Maintain WCAG AA contrast: verify aged walnut (`#3A2B24`) on parchment
  cream (`#F5EEE0`) and white text on pomegranate red before shipping —
  both should pass at body text sizes, but confirm with a contrast checker
  once real components are built.
- Do not rely on color alone for category distinction (e.g. fresh fruit vs
  preserves) — pair color with the wax-seal label text.
- Ensure focus states are visible and use the saffron gold accent color,
  not just a faint outline.

## 9. Font Loading Reference

```
Estedad
  Weights used: 400, 500, 600, 700
  Source: fontiran / Google Fonts / github.com/aminabedi68/Estedad
  License: SIL Open Font License 1.1

Alyamama
  Weights used: 600, 700, 800
  Source: Google Fonts / github.com/Mestaratype/AlYamama
  License: SIL Open Font License 1.1
```

Both are variable fonts — prefer loading the variable font file and
setting `font-variation-settings` / `font-weight` per use case instead of
loading multiple static weight files, to keep page weight down.
