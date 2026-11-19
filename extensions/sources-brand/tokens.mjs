// Eight reviewed legacy color options; data is shared by both builds and runtimes.
// Source: private Main f9e85bdd / frontend/src/theme/palettes.ts.
const catalog = [
  {
    "key": "sources-blue",
    "name": "Blue",
    "rgb": "0 119 182",
    "contrast": "#ffffff",
    "colors": {
      "50": "#eefaff",
      "100": "#caf0f8",
      "200": "#ade8f4",
      "300": "#90e0ef",
      "400": "#48cae4",
      "500": "#00b4d8",
      "600": "#0096c7",
      "700": "#0077b6",
      "800": "#023e8a",
      "900": "#03045e",
      "950": "#020238"
    }
  },
  {
    "key": "teal",
    "name": "Teal",
    "rgb": "20 184 166",
    "contrast": "#ffffff",
    "colors": {
      "50": "#f0fdfa",
      "100": "#ccfbf1",
      "200": "#99f6e4",
      "300": "#5eead4",
      "400": "#2dd4bf",
      "500": "#14b8a6",
      "600": "#0d9488",
      "700": "#0f766e",
      "800": "#115e59",
      "900": "#134e4a",
      "950": "#042f2e"
    }
  },
  {
    "key": "warm-natural",
    "name": "Warm Natural",
    "rgb": "212 163 115",
    "contrast": "#1f2937",
    "colors": {
      "50": "#fffdf0",
      "100": "#fefae0",
      "200": "#faedcd",
      "300": "#e9edc9",
      "400": "#ccd5ae",
      "500": "#d4a373",
      "600": "#b9824f",
      "700": "#95663c",
      "800": "#765132",
      "900": "#60432b",
      "950": "#332114"
    }
  },
  {
    "key": "forest-green",
    "name": "Forest Green",
    "rgb": "79 119 45",
    "contrast": "#ffffff",
    "colors": {
      "50": "#f7fbdc",
      "100": "#ecf39e",
      "200": "#d5e878",
      "300": "#b7cf5c",
      "400": "#90a955",
      "500": "#4f772d",
      "600": "#31572c",
      "700": "#1f3f20",
      "800": "#132a13",
      "900": "#0d200d",
      "950": "#061006"
    }
  },
  {
    "key": "orange",
    "name": "Orange",
    "rgb": "249 115 22",
    "contrast": "#ffffff",
    "colors": {
      "50": "#fff7ed",
      "100": "#ffedd5",
      "200": "#fed7aa",
      "300": "#fdba74",
      "400": "#fb923c",
      "500": "#f97316",
      "600": "#ea580c",
      "700": "#c2410c",
      "800": "#9a3412",
      "900": "#7c2d12",
      "950": "#431407"
    }
  },
  {
    "key": "yellow",
    "name": "Yellow",
    "rgb": "242 193 78",
    "contrast": "#1f2937",
    "colors": {
      "50": "#fffdf2",
      "100": "#fff4c7",
      "200": "#fdeba4",
      "300": "#fde68a",
      "400": "#f7d774",
      "500": "#f2c14e",
      "600": "#e0a93b",
      "700": "#c88f2a",
      "800": "#a36b1c",
      "900": "#7a4b16",
      "950": "#452708"
    }
  },
  {
    "key": "red",
    "name": "Red",
    "rgb": "239 68 68",
    "contrast": "#ffffff",
    "colors": {
      "50": "#fef2f2",
      "100": "#fee2e2",
      "200": "#fecaca",
      "300": "#fca5a5",
      "400": "#f87171",
      "500": "#ef4444",
      "600": "#dc2626",
      "700": "#b91c1c",
      "800": "#991b1b",
      "900": "#7f1d1d",
      "950": "#450a0a"
    }
  },
  {
    "key": "slate",
    "name": "Slate",
    "rgb": "100 116 139",
    "contrast": "#ffffff",
    "colors": {
      "50": "#f8fafc",
      "100": "#f1f5f9",
      "200": "#e2e8f0",
      "300": "#cbd5e1",
      "400": "#94a3b8",
      "500": "#64748b",
      "600": "#475569",
      "700": "#334155",
      "800": "#1e293b",
      "900": "#0f172a",
      "950": "#020617"
    }
  }
]

export const DEFAULT_THEME_COLOR = 'yellow'
export const THEME_COLOR_KEY = 'theme-color'
export const THEME_MODE_KEY = 'theme'
export const palettes = Object.freeze(catalog.map(p => Object.freeze({ ...p, colors: Object.freeze(p.colors) })))
export function resolvePalette(key) {
  return palettes.find(p => p.key === key) ?? palettes.find(p => p.key === DEFAULT_THEME_COLOR)
}
export function isThemeColor(key) { return palettes.some(p => p.key === key) }
export function rgb(hex) {
  return [1, 3, 5].map(offset => Number.parseInt(hex.slice(offset, offset + 2), 16)).join(' ')
}
function luminance(hex) {
  const values = rgb(hex).split(' ').map(x => {
    const v = Number(x) / 255
    return v <= 0.04045 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4
  })
  return values[0] * 0.2126 + values[1] * 0.7152 + values[2] * 0.0722
}
export function contrast(a, b) {
  const values = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (values[0] + 0.05) / (values[1] + 0.05)
}
function readableText(...backgrounds) {
  return ['#111827', '#ffffff', '#000000'].find(fg => backgrounds.every(bg => contrast(fg, bg) >= 4.5))
}
function buttonPair(palette, start) {
  for (const shade of [start, start + 100, start + 200]) {
    const from = palette.colors[shade], to = palette.colors[shade + 100]
    const foreground = readableText(from, to)
    if (foreground) return { from, to, foreground }
  }
  throw new Error('review primary button contrast')
}
export function variablesFor(key) {
  const palette = resolvePalette(key)
  const vars = {}
  for (const [shade, hex] of Object.entries(palette.colors)) {
    vars[`--color-primary-${shade}`] = hex
    vars[`--color-primary-${shade}-rgb`] = rgb(hex)
    vars[`--color-primary-${shade}-contrast`] = readableText(hex)
  }
  const button = buttonPair(palette, 500), hover = buttonPair(palette, 600)
  Object.assign(vars, {
    '--color-primary-rgb': palette.rgb,
    '--color-primary-contrast': vars['--color-primary-500-contrast'],
    '--sources-brand-foreground': vars['--color-primary-500-contrast'],
    '--sources-button-from': button.from,
    '--sources-button-to': button.to,
    '--sources-button-foreground': button.foreground,
    '--sources-button-hover-from': hover.from,
    '--sources-button-hover-to': hover.to,
    '--sources-button-hover-foreground': hover.foreground,
  })
  return vars
}

// RGB variables keep Tailwind's opacity variants (e.g. bg-primary-500/20) working.
const cssVariables = variablesFor(DEFAULT_THEME_COLOR)
const primary = Object.fromEntries(Object.entries(resolvePalette(DEFAULT_THEME_COLOR).colors)
  .map(([shade, hex]) => [shade, `rgb(var(--color-primary-${shade}-rgb, ${rgb(hex)}) / <alpha-value>)`]))
const effects = {
  glow: '0 0 20px rgb(var(--color-primary-rgb) / 0.25)',
  glowLarge: '0 0 40px rgb(var(--color-primary-rgb) / 0.35)',
  glowEnd: '0 0 30px rgb(var(--color-primary-rgb) / 0.4)',
  gradient: 'linear-gradient(135deg, var(--color-primary-500), var(--color-primary-600))',
  mesh: 'radial-gradient(at 40% 20%, rgb(var(--color-primary-rgb) / 0.12) 0px, transparent 50%), radial-gradient(at 80% 0%, rgb(var(--color-primary-400-rgb) / 0.08) 0px, transparent 50%), radial-gradient(at 0% 50%, rgb(var(--color-primary-rgb) / 0.08) 0px, transparent 50%)',
}
export default { primary, cssVariables, effects }
