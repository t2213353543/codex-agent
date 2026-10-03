import { createApp } from 'vue'
import 'element-plus/theme-chalk/base.css'
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'
import 'element-plus/es/components/loading/style/css'
// Dialogs and programmatic overlays can open before their route's CSS chunk.
// Load their shared controls once, before the application theme overrides.
import 'element-plus/es/components/dialog/style/css'
import 'element-plus/es/components/drawer/style/css'
import 'element-plus/es/components/form/style/css'
import 'element-plus/es/components/form-item/style/css'
import 'element-plus/es/components/input-number/style/css'
import 'element-plus/es/components/select/style/css'
import 'element-plus/es/components/checkbox/style/css'
import 'element-plus/es/components/checkbox-group/style/css'
import 'element-plus/es/components/radio/style/css'
import 'element-plus/es/components/radio-button/style/css'
import 'element-plus/es/components/radio-group/style/css'
import 'element-plus/es/components/switch/style/css'
import 'element-plus/es/components/alert/style/css'
import 'element-plus/es/components/tooltip/style/css'
import 'element-plus/es/components/popover/style/css'
import 'element-plus/es/components/dropdown/style/css'
import 'element-plus/theme-chalk/dark/css-vars.css'
import { Cpu, Odometer, Collection, Key, Connection, User, Document, Setting, DArrowLeft, DArrowRight, Expand, Fold, Refresh, RefreshRight, RefreshLeft, ArrowDown, Close, Upload, Search, Plus, Lock } from '@element-plus/icons-vue'
import App from './App.vue'
import router from './router'
import { applyTheme } from './theme'
import './style.css'
import './styles/console-design.css'
import './styles/console-density.css'
import './styles/console-workflows.css'

applyTheme()

const app = createApp(App)
app.use(router)
for (const [name, component] of Object.entries({ Cpu, Odometer, Collection, Key, Connection, User, Document, Setting, DArrowLeft, DArrowRight, Expand, Fold, Refresh, RefreshRight, RefreshLeft, ArrowDown, Close, Upload, Search, Plus, Lock })) {
  app.component(name, component)
}
app.mount('#app')
