export type ThemeColor = 'sources-blue' | 'teal' | 'warm-natural' | 'forest-green' | 'orange' | 'yellow' | 'red' | 'slate'
export type Shade = 50 | 100 | 200 | 300 | 400 | 500 | 600 | 700 | 800 | 900 | 950
export interface ThemePalette {
  readonly key: ThemeColor
  readonly name: string
  readonly rgb: string
  readonly contrast: string
  readonly colors: Readonly<Record<Shade, string>>
}
export const DEFAULT_THEME_COLOR: ThemeColor
export const THEME_COLOR_KEY: 'theme-color'
export const THEME_MODE_KEY: 'theme'
export const palettes: readonly ThemePalette[]
export function resolvePalette(key: unknown): ThemePalette
export function isThemeColor(key: unknown): key is ThemeColor
export function rgb(hex: string): string
export function contrast(a: string, b: string): number
export function variablesFor(key: unknown): Record<string, string>
declare const tokens: {
  primary: Record<Shade, string>
  cssVariables: Record<string, string>
  effects: { glow: string; glowLarge: string; glowEnd: string; gradient: string; mesh: string }
}
export default tokens
