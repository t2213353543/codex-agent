<script setup>
import { computed } from 'vue'
import { inferenceRuntime } from '../utils/bpsRuntime'
const props = defineProps({ inference: Object, advanced: Boolean, compact: Boolean, mode: String, modes: Array, disabled: Boolean, saving: Boolean })
const emit = defineEmits(['change-mode'])
const runtime = computed(() => inferenceRuntime(props.inference))
</script>

<template>
  <section class="powerpoint-service" :class="{ 'service-compact': compact }" aria-label="推理服务配置">
    <div class="service-heading"><h3>推理服务</h3><span>{{ runtime.label }}</span></div>
    <div v-if="modes?.length" class="mode-selector">
      <label for="inference-mode">运行模式</label>
      <el-select id="inference-mode" :model-value="mode" :disabled="disabled" :loading="saving" aria-label="运行模式" @change="value => emit('change-mode', value)">
        <el-option v-for="option in modes" :key="option.value" :label="option.label" :value="option.value" />
      </el-select>
      <p class="service-note">保存后用于新请求，正在生成的请求继续使用原模式。切换后请新建客户端会话，避免混用不同服务的加密上下文。</p>
    </div>
    <p class="service-note">Codex 及兼容客户端通过 API 管理会话、压缩和本地工具。可用模型以账号实际返回为准。</p>
    <dl class="service-fields">
      <div><dt>推理入口</dt><dd>{{ runtime.known ? runtime.label : '未取得当前配置' }}</dd></div>
      <div><dt>网页搜索</dt><dd>{{ runtime.search }}</dd></div>
      <div><dt>Codex 推理回退</dt><dd>{{ runtime.codexFallback }}</dd></div>
    </dl>
    <details v-if="advanced && runtime.mode === 'powerpoint_bps'" class="service-advanced">
      <summary>高级部署配置</summary>
      <p class="service-note">以下为当前进程读取的配置。修改部署环境变量后重启服务，再刷新此页核对。</p>
      <dl class="service-fields">
        <div><dt>BPS_BASE_URL</dt><dd><code>{{ runtime.baseURL || '未提供' }}</code></dd></div>
        <div><dt>BPS_TOOLS_VERSION</dt><dd><code>{{ runtime.toolsVersion || '未提供' }}</code></dd></div>
        <div><dt>BPS_WEB_SEARCH</dt><dd>{{ runtime.search }}</dd></div>
      </dl>
      <p class="service-note">请求身份由 PowerPoint 服务固定配置。缓存键由网关按调用密钥和账号隔离，不提供关闭隔离的选项。</p>
    </details>
  </section>
</template>

<style scoped>
.service-compact .service-note{margin:8px 0;line-height:1.5}.service-compact .service-fields{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:8px}.service-compact .service-fields>div{display:flex;flex-wrap:wrap;align-content:start;gap:4px 8px;padding:4px 0;border:0}.service-compact .service-fields dt{font-size:11px}.service-compact .service-fields dd{font-size:12px}@media(max-width:620px){.service-compact .service-fields{grid-template-columns:1fr}.service-compact .service-fields>div{padding:2px 0;justify-content:space-between}}
.mode-selector{display:grid;gap:10px;margin-top:16px}.mode-selector>label{font-size:12px;color:var(--text-2)}.mode-selector .el-select{width:min(100%,360px)}
.powerpoint-service{min-width:0}.service-heading{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap}.service-heading h3{margin:0;font-size:14px;font-weight:600}.service-heading>span{font-size:11px;color:var(--primary)}.service-note{margin:12px 0;color:var(--text-3);font-size:12px;line-height:1.8}.service-fields{margin:0;font-size:12px}.service-fields>div{display:grid;grid-template-columns:minmax(110px,155px) minmax(0,1fr);gap:12px;padding:12px 0;border-bottom:1px solid var(--card-border)}.service-fields dt{color:var(--text-3)}.service-fields dd{margin:0;overflow-wrap:anywhere}.service-fields code{font-size:11px}.service-advanced{margin-top:18px}.service-advanced summary{cursor:pointer;font-size:12px;color:var(--text-2)}@media(max-width:480px){.service-fields>div{grid-template-columns:1fr;gap:6px}}
</style>
