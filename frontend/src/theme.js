import { readPreference, writePreference } from './utils/preferences.js'

export const themes = [
  { value: 'light', label: '留白', colors: ['#187c64', '#ffffff', '#f6f8f7'] },
  { value: 'dark', label: '夜航', colors: ['#b0a6ff', '#1a1c21', '#121316'] },
]

function normalizeTheme(value) {
  if (value === 'light' || value === 'ocean' || value === 'mint') return 'light'
  return 'dark'
}

export function getTheme() {
  return normalizeTheme(readPreference('theme', 'dark', value => ['light', 'dark', 'ocean', 'mint'].includes(value)))
}

export function applyTheme(value) {
  const theme = normalizeTheme(value ?? getTheme())
  writePreference('theme', theme)
  document.documentElement.dataset.theme = theme
  document.documentElement.classList.toggle('dark', theme === 'dark')
  window.dispatchEvent(new CustomEvent('theme-change', { detail: theme }))
  return theme
}
