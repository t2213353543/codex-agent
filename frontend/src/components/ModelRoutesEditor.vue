<template>
  <div class="model-routes-editor">
    <div class="editor-header">
      <div class="header-info">
        <div class="header-label">模型映射规则</div>
        <div class="header-desc">将客户端请求的模型名映射到实际使用的模型。每条规则指定最终目标，请求期间不探测或轮询。</div>
      </div>
      <el-button size="small" :disabled="disabled" @click="addRule">
        <el-icon><Plus /></el-icon>
        <span>添加规则</span>
      </el-button>
    </div>

    <div class="available-targets">
      <span class="targets-label">已核实目标：</span>
      <span class="targets-list">{{ targets.join('、') || '无' }}</span>
    </div>

    <div class="rules-list">
      <TransitionGroup name="rule">
        <div v-for="(rule, index) in rules" :key="rule.id" class="rule-item">
          <div class="rule-index">{{ index + 1 }}</div>
          <div class="rule-fields">
            <div class="rule-field">
              <label :for="`rule-source-${rule.id}`" class="field-label">请求模型</label>
              <el-input
                :id="`rule-source-${rule.id}`"
                v-model="rule.source"
                placeholder="模型名或 * (兜底)"
                :disabled="disabled"
                size="small"
                @input="emitUpdate"
              />
            </div>
            <div class="rule-arrow">→</div>
            <div class="rule-field">
              <label :for="`rule-target-${rule.id}`" class="field-label">实际模型</label>
              <el-select
                :id="`rule-target-${rule.id}`"
                v-model="rule.target"
                placeholder="选择目标模型"
                :disabled="disabled"
                size="small"
                filterable
                allow-create
                @change="emitUpdate"
              >
                <el-option v-for="target in targets" :key="target" :label="target" :value="target" />
              </el-select>
            </div>
          </div>
          <el-button
            :disabled="disabled"
            size="small"
            text
            @click="removeRule(index)"
          >
            <el-icon><Delete /></el-icon>
          </el-button>
        </div>
      </TransitionGroup>

      <div v-if="rules.length === 0" class="empty-state">
        <el-icon class="empty-icon"><InfoFilled /></el-icon>
        <span>暂无规则，点击"添加规则"开始配置</span>
      </div>
    </div>

    <div class="editor-footer">
      <el-icon class="footer-icon"><WarningFilled /></el-icon>
      <span class="footer-text">规则按顺序匹配，请确保包含 * 作为兜底规则</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { ref, watch, computed } from 'vue'
import { Plus, Delete, InfoFilled, WarningFilled } from '@element-plus/icons-vue'

interface Rule {
  id: number
  source: string
  target: string
}

interface Props {
  modelValue: string
  targets: string[]
  disabled?: boolean
}

const props = withDefaults(defineProps<Props>(), {
  disabled: false,
})

const emit = defineEmits<{
  'update:modelValue': [value: string]
}>()

let nextId = 0
const rules = ref<Rule[]>([])

// 解析字符串为规则数组
function parseRules(value: string): Rule[] {
  if (!value.trim()) return []

  const lines = value.split('\n')
  const parsed: Rule[] = []

  for (const line of lines) {
    const trimmed = line.trim()
    if (!trimmed || trimmed.startsWith('#')) continue

    const match = trimmed.match(/^([^=]+)=(.+)$/)
    if (match) {
      parsed.push({
        id: nextId++,
        source: match[1].trim(),
        target: match[2].trim(),
      })
    }
  }

  return parsed
}

// 将规则数组转换为字符串
function serializeRules(rules: Rule[]): string {
  return rules
    .filter(r => r.source && r.target)
    .map(r => `${r.source}=${r.target}`)
    .join('\n')
}

// 初始化规则
watch(
  () => props.modelValue,
  (value) => {
    rules.value = parseRules(value)
  },
  { immediate: true }
)

function addRule() {
  rules.value.push({
    id: nextId++,
    source: '',
    target: '',
  })
  emitUpdate()
}

function removeRule(index: number) {
  rules.value.splice(index, 1)
  emitUpdate()
}

function emitUpdate() {
  emit('update:modelValue', serializeRules(rules.value))
}
</script>

<style scoped>
.model-routes-editor { display: grid; gap: 16px; }

.editor-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
}

.header-info { flex: 1; display: grid; gap: 6px; }

.header-label {
  font-size: 14px;
  font-weight: 500;
  color: var(--surface-900);
}

.header-desc {
  font-size: 13px;
  line-height: 1.5;
  color: var(--surface-600);
}

.available-targets {
  display: flex;
  align-items: baseline;
  gap: 8px;
  padding: 10px 14px;
  background: var(--surface-50);
  border-radius: 8px;
  font-size: 13px;
}

.targets-label {
  font-weight: 500;
  color: var(--surface-700);
  white-space: nowrap;
}

.targets-list {
  color: var(--surface-600);
  flex: 1;
  overflow-wrap: anywhere;
}

.rules-list {
  display: grid;
  gap: 10px;
  min-height: 60px;
}

.rule-item {
  display: grid;
  grid-template-columns: 28px 1fr auto;
  align-items: center;
  gap: 12px;
  padding: 12px;
  background: var(--surface-50);
  border: 1px solid var(--surface-200);
  border-radius: 8px;
  transition: all 0.2s ease;
}

.rule-item:hover {
  border-color: var(--surface-300);
  box-shadow: 0 2px 8px rgb(0 0 0 / 0.04);
}

.rule-index {
  width: 28px;
  height: 28px;
  display: flex;
  align-items: center;
  justify-content: center;
  background: var(--surface-100);
  border-radius: 6px;
  font-size: 13px;
  font-weight: 600;
  color: var(--surface-600);
}

.rule-fields {
  display: grid;
  grid-template-columns: 1fr auto 1fr;
  align-items: end;
  gap: 12px;
  flex: 1;
}

.rule-field {
  display: grid;
  gap: 6px;
}

.field-label {
  font-size: 12px;
  font-weight: 500;
  color: var(--surface-700);
}

.rule-arrow {
  font-size: 18px;
  color: var(--surface-400);
  padding-bottom: 4px;
}

.empty-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 8px;
  padding: 32px;
  background: var(--surface-50);
  border: 1px dashed var(--surface-300);
  border-radius: 8px;
  color: var(--surface-500);
  font-size: 13px;
}

.empty-icon {
  font-size: 18px;
  color: var(--surface-400);
}

.editor-footer {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 14px;
  background: var(--warning-50);
  border-radius: 8px;
}

.footer-icon {
  font-size: 16px;
  color: var(--warning-600);
  flex-shrink: 0;
}

.footer-text {
  font-size: 13px;
  color: var(--warning-700);
  line-height: 1.5;
}

.rule-enter-active,
.rule-leave-active {
  transition: all 0.3s ease;
}

.rule-enter-from {
  opacity: 0;
  transform: translateY(-10px);
}

.rule-leave-to {
  opacity: 0;
  transform: translateX(20px);
}

.rule-move {
  transition: transform 0.3s ease;
}
</style>
