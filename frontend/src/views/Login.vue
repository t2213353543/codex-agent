<script setup>
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Key, Lock, ArrowRight, Connection, DataAnalysis, Setting } from '@element-plus/icons-vue'
import { loginSession } from '../api'
import { managementPages } from '../utils/navigation'
const router = useRouter()
const route = useRoute()
const adminKey = ref('')
const loading = ref(false)
const errorMessage = ref('')
const capsLock = ref(false)
function updateCapsLock(event) { capsLock.value = event.getModifierState?.('CapsLock') === true }
async function login() {
  if (loading.value) return
  const key = adminKey.value.trim()
  if (!key) { errorMessage.value = '请输入 admin key'; return }
  loading.value = true
  errorMessage.value = ''
  try {
    await loginSession(key)
    adminKey.value = ''
    ElMessage.success('登录成功')
    const target = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
    const allowed = managementPages.map(page => page.path)
    await router.replace(target.startsWith('/') && !target.startsWith('//') && allowed.includes(router.resolve(target).path) ? target : '/')
  } catch (error) { errorMessage.value = error.message || '登录失败，请重试' }
  finally { loading.value = false }
}
</script>
<template>
  <main class="login-experience" aria-label="高级反代系统 登录">
    <section class="login-brand">
      <div class="brand-lockup"><span class="brand-symbol" aria-hidden="true">P<span>+</span></span><div><strong class="powerpoint-login-brand">高级反代系统</strong><span>GATEWAY · 管理控制台</span></div></div>
      <div class="brand-message"><span class="brand-eyebrow">一个井然有序的工作区</span><h2>每次请求，<br />都有清晰的全貌。</h2><p>连接你的账号与应用，让资源配置、运行观察和问题排查回到同一个地方。</p></div>
      <div class="brand-workflow" aria-hidden="true">
        <div class="workflow-node"><el-icon><Connection /></el-icon><span>账号资源</span></div>
        <span class="workflow-link"><i></i></span>
        <div class="workflow-hub"><span>C_</span></div>
        <span class="workflow-link"><i></i></span>
        <div class="workflow-node"><el-icon><DataAnalysis /></el-icon><span>请求记录</span></div>
      </div>
      <div class="brand-features">
        <div><el-icon><Connection /></el-icon><span><strong>资源管理</strong><small>账号、分组、API KEY 与代理</small></span></div>
        <div><el-icon><DataAnalysis /></el-icon><span><strong>运行观察</strong><small>请求用量、缓存表现与运行诊断</small></span></div>
        <div><el-icon><Setting /></el-icon><span><strong>有据可查</strong><small>请求详情与管理操作记录</small></span></div>
      </div>
      <div class="brand-footer"><span class="brand-dot" /> 专注每一次调用 <span>CODEX / CONSOLE</span></div>
    </section>
    <section class="login-card">
      <div class="login-mobile-brand"><span class="mobile-brand-symbol">P+</span>高级反代系统</div>
      <div class="login-heading-icon" aria-hidden="true"><el-icon><Lock /></el-icon></div>
      <div class="login-eyebrow">WELCOME BACK</div>
      <h1>登录管理控制台</h1>
      <p class="login-sub">欢迎回来，使用管理密钥继续。</p>
      <el-form label-position="top" @submit.prevent="login">
        <el-form-item label="管理密钥">
          <el-input v-model="adminKey" aria-label="管理密钥" type="password" size="large" placeholder="请输入 admin key" show-password autocomplete="current-password" :disabled="loading" :aria-invalid="!!errorMessage" :aria-describedby="errorMessage ? 'login-error login-key-help' : 'login-key-help'" @keydown="updateCapsLock" @keyup="updateCapsLock" @blur="capsLock = false">
            <template #prefix><el-icon><Key /></el-icon></template>
          </el-input>
        </el-form-item>
        <p id="login-key-help" class="login-key-help">请输入本系统的 admin key，API 调用密钥不能用于管理登录。</p>
        <p v-if="capsLock" class="caps-lock-note" role="status">大写锁定已开启，请核对密钥大小写。</p>
        <p v-if="errorMessage" id="login-error" class="form-error login-error" role="alert">{{ errorMessage }}</p>
        <el-button native-type="submit" type="primary" size="large" class="login-submit" :loading="loading">登 录<el-icon v-if="!loading" class="login-arrow"><ArrowRight /></el-icon></el-button>
      </el-form>
      <div class="login-session-note"><el-icon><Lock /></el-icon><p>登录使用短期安全会话，浏览器不保存管理密钥。会话过期后需重新登录。</p></div>
      <p class="login-access-help">尚未获得管理密钥？请联系此系统的管理员。</p>
    </section>
  </main>
</template>

<style scoped>
.login-experience { width: min(1040px, calc(100vw - 64px)); display: grid; grid-template-columns: 1.1fr 1fr; border: 1px solid var(--card-border); border-radius: 18px; overflow: hidden; background: var(--card-bg); margin: 80px 0 36px; }
.login-brand { position: relative; display: flex; flex-direction: column; min-width: 0; padding: 39px 43px 27px; border-right: 1px solid var(--card-border); background: color-mix(in srgb, var(--primary-soft) 45%, var(--bg)); color: var(--text-1); }
.brand-lockup { display: flex; align-items: center; gap: 12px; }
.brand-symbol { position: relative; display: grid; place-content: center; width: 39px; height: 39px; padding-right: 4px; border: 1px solid color-mix(in srgb, var(--primary) 25%, var(--card-border)); border-radius: 11px; background: var(--card-bg); color: var(--primary); font: 600 23px monospace; }
.brand-symbol span { position: absolute; right: 5px; bottom: 7px; color: var(--primary); }
.brand-lockup strong { display: block; font-size: 26px; font-weight: 650; letter-spacing: -1.2px; line-height: 1; }
.brand-lockup strong.powerpoint-login-brand{font-size:20px;letter-spacing:-.6px;line-height:1.3}.login-mobile-brand{overflow-wrap:anywhere}
.brand-punctuation { color: var(--primary); }
.brand-lockup div > span { display: block; margin-top: 6px; color: var(--text-3); font-size: 7px; font-weight: 500; letter-spacing: 1.3px; }
.brand-message { margin: 55px 0 25px; }
.brand-eyebrow { color: var(--primary); font-size: 10px; letter-spacing: 1.5px; }
.brand-message h2 { margin: 17px 0 16px; font-size: 33px; line-height: 1.55; font-weight: 600; letter-spacing: -.9px; }
.brand-message p { max-width: 305px; margin: 0; color: var(--text-2); font-size: 12px; line-height: 1.95; }
.brand-workflow { display: flex; align-items: center; justify-content: center; min-width: 0; padding: 21px 16px; margin: 7px 0 28px; border: 1px solid color-mix(in srgb, var(--primary) 12%, var(--card-border)); border-radius: 11px; background: color-mix(in srgb, var(--card-bg) 55%, transparent); }
.workflow-node { display: flex; flex-direction: column; align-items: center; gap: 9px; flex-shrink: 0; color: var(--text-2); font-size: 8px; }
.workflow-node .el-icon { display: grid; place-items: center; width: 34px; height: 34px; border: 1px solid var(--card-border); border-radius: 8px; color: var(--primary); background: var(--card-bg); font-size: 17px; }
.workflow-link { display: block; position: relative; flex: 1; height: 1px; margin: -15px 10px 0; border-top: 1px dashed color-mix(in srgb, var(--primary) 32%, var(--card-border)); }
.workflow-link i { position: absolute; right: -1px; top: -3px; width: 4px; height: 4px; border-top: 1px solid var(--primary); border-right: 1px solid var(--primary); transform: rotate(45deg); }
.workflow-hub { display: grid; place-items: center; flex-shrink: 0; width: 55px; height: 55px; margin-top: -15px; border: 1px solid color-mix(in srgb, var(--primary) 28%, var(--card-border)); border-radius: 16px; background: var(--primary-soft); color: var(--primary); font: 600 23px monospace; }
.brand-features { display: grid; gap: 16px; }
.brand-features > div { display: flex; gap: 12px; align-items: center; }
.brand-features .el-icon { display: grid; place-items: center; width: 28px; height: 28px; font-size: 16px; color: var(--primary); }
.brand-features strong { display: block; color: var(--text-2); font-size: 11px; font-weight: 500; }
.brand-features small { display: block; margin-top: 4px; color: var(--text-3); font-size: 10px; }
.brand-footer { display: flex; align-items: center; gap: 7px; margin-top: 38px; padding-top: 18px; border-top: 1px solid var(--card-border); color: var(--text-3); font-size: 9px; }
.brand-footer > span:last-child { margin-left: auto; font-size: 7px; letter-spacing: 1.2px; }
.brand-dot { height: 5px; width: 5px; border-radius: 50%; background: var(--primary); }
.login-card { width: auto; min-width: 0; align-self: center; padding: 53px 46px 42px; border: 0; border-radius: 0; background: var(--card-bg); box-shadow: none; }
.login-heading-icon { display: grid; place-content: center; height: 44px; width: 44px; margin-bottom: 25px; border: 1px solid var(--card-border); border-radius: 12px; background: var(--bg); color: var(--primary); font-size: 20px; }
.login-eyebrow { margin-bottom: 10px; color: var(--text-3); font-size: 8px; font-weight: 500; letter-spacing: 1.8px; }
.login-card h1 { margin: 0 0 11px; text-align: left; color: var(--text-1); font-size: 24px; font-weight: 600; letter-spacing: -.6px; }
.login-card .login-sub { margin: 0 0 34px; color: var(--text-3); text-align: left; font-size: 12px; line-height: 1.7; }
.login-card :deep(.el-form-item__label) { color: var(--text-2); font-size: 11px; font-weight: 500; }
.login-card :deep(.el-form-item) { margin-bottom: 10px; }
.login-card :deep(.el-input__wrapper) { min-height: 43px; border-radius: 8px; background: var(--bg); }
.login-card :deep(.el-input__inner) { font-size: 12px; }
.login-key-help, .login-access-help { color: var(--text-3); font-size: 10px; line-height: 1.9; }
.login-key-help { margin: 9px 0 0; }
.login-submit { width: 100%; margin-top: 24px; min-height: 44px; border-radius: 8px; font-size: 12px; font-weight: 600; }
.login-arrow { margin-left: 12px; }
.login-error { padding: 10px 12px; margin-top: 14px; border: 1px solid color-mix(in srgb, var(--danger) 30%, var(--card-border)); border-radius: 7px; background: color-mix(in srgb, var(--danger) 7%, var(--card-bg)); color: var(--danger); font-size: 11px; line-height: 1.7; overflow-wrap: anywhere; }
.caps-lock-note { margin-top: 9px; color: var(--warning); font-size: 10px; }
.login-session-note { display: flex; align-items: flex-start; gap: 9px; padding-top: 23px; margin-top: 26px; border-top: 1px solid var(--card-border); color: var(--text-3); }
.login-session-note .el-icon { margin-top: 4px; flex-shrink: 0; color: var(--text-3); }
.login-session-note p { margin: 0; font-size: 10px; line-height: 1.9; }
.login-access-help { margin-top: 23px; }
.login-mobile-brand { display: none; }
@media (max-width: 1000px) { .login-experience { width: min(900px, calc(100vw - 48px)); grid-template-columns: 1fr 1fr; } .login-brand { padding: 34px 30px 25px; } .login-card { padding: 44px 33px 35px; } .brand-message h2 { font-size: 29px; } .brand-message { margin-top: 47px; } }
@media (max-width: 760px) { .login-experience { grid-template-columns: 1fr; max-width: 440px; border-radius: 15px; margin-top: 82px; } .login-brand { display: none; } .login-card { padding: 34px; } .login-mobile-brand { display: flex; align-items: center; gap: 9px; margin-bottom: 35px; color: var(--text-1); font-size: 13px; font-weight: 600; } .mobile-brand-symbol { display: grid; place-items: center; width: 29px; height: 29px; border: 1px solid var(--card-border); border-radius: 8px; color: var(--primary); background: var(--primary-soft); font: 600 16px monospace; } .login-heading-icon { display: none; } .login-card h1 { font-size: 24px; } }
@media (max-width: 390px) { .login-experience { width: calc(100vw - 28px); margin-top: 74px; } .login-card { padding: 28px 24px; } .login-card h1 { font-size: 22px; } .login-card .login-sub { margin-bottom: 28px; } .login-mobile-brand { margin-bottom: 30px; } }
</style>
