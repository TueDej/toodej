# DESIGN.md — Toodej

Design system for the Toodej storefront. The site is in Persian and reads right-to-left (RTL); this document is in English for the dev team.

## 1. Brand Concept

Warmth and provenance over polish. Feels like stepping into a family orchard — not a templated Shopify theme. Handmade over corporate: slightly rotated cards, hand-drawn dividers, paper grain texture.

## 2. Color Palette

| Token | Hex | Usage |
|-------|-----|-------|
| Pomegranate | `#9E2A2B` | Primary actions, buttons, prices |
| Fig | `#5B3A5C` | Secondary accent, headings, badges |
| Forest | `#3F5D42` | Success states, nav underlines, footer bg |
| Parchment | `#F5EEE0` | Main background |
| Sand | `#EBDFC9` | Section backgrounds, card fills |
| Walnut | `#3A2B24` | Primary text |
| Clay | `#8C6F5E` | Secondary text, metadata |
| Saffron | `#C98A2C` | Highlights, hover states, focus rings |
| Rose | `#C97064` | Accent for traditional/paste products |
| Line | `#E2D2B8` | Borders, dividers |

**Rules:** Backgrounds are never pure white. Text is never pure black. Gradients are avoided except for hero image overlays (walnut-to-transparent).

## 3. Typography

**IBM Plex Sans Arabic** — used for all text (body, UI, headings, display). Supports Farsi script with real Arabic/Persian glyph shaping. Loaded via Google Fonts.

- Body: 16px, line-height 1.8
- Display/hero: 40px mobile → 64px desktop
- H1: 32px mobile → 48px desktop
- H2: 24px mobile → 32px desktop
- Small/caption: 13px

**Persian-specific:** Use Persian digits (۰-۹) for prices and quantities. Never justify body text. Avoid italics. Generous line-height (1.7-1.9) for diacritics.

## 4. Layout & Direction

- Full RTL layout (`dir="rtl"` on `<html>`)
- Full-viewport sections: hero (100dvh), seasonal slider (min-height 100dvh), footer (min-height 100vh)
- 12-column grid on desktop, 4-column on mobile, generous gutters
- Loose section spacing (96-140px on desktop)
- Soft rounded corners (1.2rem card radius)

## 5. Key Components

- **paper-card:** Gradient surface (parchment → warm white), subtle border, soft shadow
- **product-card:** Slight rotation on hover (0.5-1°), lifts 4px, shadow intensifies
- **wax-seal:** Circular badge with radial gradient, rotated -8°, for "fresh"/"handmade" tags
- **scroll-cue:** Bouncing double-chevron arrow at hero bottom
- **grain:** Fixed paper/linen texture overlay (5% opacity, multiply blend)

## 6. Seasonal Taxonomy

Categories use a product-based taxonomy surfaced in header nav and `/products/{slug}` routes:
- `fig` (انجیر)
- `pomegranate` (انار)
- `traditional` (محصولات سنتی/خانگی)
- `test` (test)

## 7. Motion

- **Parallax slider:** Background scales (Ken Burns-style) while text rises into place on a faster timeline
- **Card hover:** Gentle lift + tilt (300-500ms, ease-out)
- **Scroll cue:** Slow bounce animation (1.6s loop)
- Avoid bouncy/springy micro-interactions

## 8. Accessibility

- WCAG AA contrast: walnut on parchment, white on pomegranate
- Focus states: 2px saffron outline, 3px offset
- Pair color with text labels (never rely on color alone)
- Respect `prefers-reduced-motion` (disables transforms)
