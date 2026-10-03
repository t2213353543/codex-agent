<script setup>
defineProps({ name: String, size: { type: [Number, String], default: 19 } })
const paths = {
  'chevron-down': 'm5 9 7 7 7-7', 'chevron-left': 'm15 5-7 7 7 7',
  audit: 'M9 3h10v18H5V7 M9 3v4H5 M9 12h6 M9 16h6',
  overview: 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z',
  accounts: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M16 3a4 4 0 0 1 0 8 M22 21v-2a4 4 0 0 0-3-3.87 M13 7a4 4 0 1 1-8 0 4 4 0 0 1 8 0',
  groups: 'M3 7V5a2 2 0 0 1 2-2h4l2 3h8a2 2 0 0 1 2 2v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7z M3 10h18',
  key: 'M14 7a5 5 0 1 1-4 8L3 22l-2-2 3-3v-3h3l3-3 M17 7h.01',
  globe: 'M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0 M3 12h18 M12 3a17 17 0 0 1 0 18 17 17 0 0 1 0-18',
  logs: 'M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z M14 2v6h6 M8 13h8 M8 17h5',
  settings: 'M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8 M10 2h4l1 3 3 1 3 1v4l-2 1-1 3 1 3-3 3-3-1-3 1-2-3H5l-2-3 1-3-1-3 3-2 1-3z',
  search: 'M21 21l-5-5 M18 10.5a7.5 7.5 0 1 1-15 0 7.5 7.5 0 0 1 15 0',
  chevron: 'm9 5 7 7-7 7', arrow: 'M4 12h16 M14 6l6 6-6 6', plus:'M12 5v14 M5 12h14', check:'m5 12 4 4L19 6', close:'m6 6 12 12 M6 18 18 6',
  menu:'M4 6h16 M4 12h16 M4 18h16', moon:'M21 12.8A9 9 0 0 1 11.2 3 9 9 0 1 0 21 12.8', sun:'M12 3V1 M12 23v-2 M3 12H1 M23 12h-2 M4 4l2 2 M18 18l2 2 M4 20l2-2 M18 6l2-2 M17 12a5 5 0 1 1-10 0 5 5 0 0 1 10 0',
  help:'M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3 M12 17h.01 M22 12a10 10 0 1 1-20 0 10 10 0 0 1 20 0',
  bell:'M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9 M10 21h4',
  code:'m8 5-7 7 7 7 M16 5l7 7-7 7 M14 3l-4 18', activity:'M2 12h5l3-8 4 16 3-8h5', grid:'M3 3h18v18H3z M3 10h18 M10 10v11',
  download:'M12 3v12 m-5-5 5 5 5-5 M4 16v5h16v-5', filter:'M4 5h16 M7 12h10 M10 19h4', refresh:'M20 7a9 9 0 0 0-15-2L2 8 M2 2v6h6 M4 17a9 9 0 0 0 15 2l3-3 M22 22v-6h-6',
  copy:'M9 8V3h12v13h-5 M3 8h13v13H3z', more:'M5 12h.01 M12 12h.01 M19 12h.01', logout:'M9 3H4v18h5 M9 12h12 m-5-5 5 5-5 5', shield:'M12 2 3 6v6c0 5 9 10 9 10s9-5 9-10V6z m-5 10 3 3 5-6', zap:'m13 2-9 12h7l-1 8L21 9h-8z',
}
</script>
<template><svg :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" class="console-icon"><path :d="paths[name] || paths.overview" /></svg></template>
