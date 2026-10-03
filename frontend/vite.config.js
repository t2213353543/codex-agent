import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'

export default defineConfig({
  plugins: [vue(), Components({ dts: false, resolvers: [ElementPlusResolver()] })],
  build: {
    rollupOptions: {
      output: {
        // Keep the reusable canvas renderer separate from ECharts' registered
        // chart types. Both load only when a chart page needs them.
        manualChunks(id) {
          if (id.replaceAll('\\', '/').includes('/node_modules/zrender/')) return 'chart-renderer'
        },
      },
    },
  },
  server: {
    proxy: Object.fromEntries(
      ['/api', '/v1', '/health'].map((path) => [path, 'http://127.0.0.1:18879']),
    ),
  },
})
