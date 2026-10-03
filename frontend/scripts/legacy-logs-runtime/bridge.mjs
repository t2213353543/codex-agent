// The aliases below belong to one immutable, verified production artifact.
// Never infer aliases from another build: every referenced file is checked
// against that release's SHA-256 manifest before this bridge is used.
export const legacyRelease = '7ca36331d1089e9a039f0949526dcb1a65fc28b2985a0f0521620646442f207b'
export const legacyManifestSHA = '02033ebc526fb83ea4b5c4f58c0a0a9737c155b1d26190b5ac7e5883b4bc93c4'
export const legacyMain = 'index-D--MHP3C.js'
export const legacyLogs = 'Logs-ZPqLUryO.js'
export const legacyScope = '73e237d0'

const main = legacyMain
const helper = '_plugin-vue_export-helper-D_ariHHy.js'

export const vueExports = {
  ref: 'j', computed: 'K', nextTick: 'M', watch: 'B', reactive: 'ah',
  onMounted: 'C', onUnmounted: 'G', onBeforeUnmount: '_', defineComponent: 'p',
  openBlock: 'o', createElementBlock: 'c', createElementVNode: 'a',
  createVNode: 'b', createBlock: 'H', createTextVNode: 'i', createCommentVNode: 'g',
  toDisplayString: 't', unref: 'x', withCtx: 'w', Fragment: 'I', renderList: 'J',
  normalizeClass: 'v', normalizeStyle: 'z', withDirectives: 'Y',
  withModifiers: 'd', withKeys: 'aL', resolveComponent: 'r', resolveDirective: 'bQ',
  resolveDynamicComponent: 'S', renderSlot: 'y', mergeProps: 'aj',
  vShow: 'bk', Teleport: 'aM', Transition: 'be',
}

export const elementExports = {
  ElButton: [main, 'h'], ElInput: [main, 'E'], ElDrawer: [main, 'aA'],
  ElMessage: [main, 'k'], ElIcon: [main, 'f'],
  ElAlert: [helper, 'E'],
  ElSelect: ['el-select-DfbR1ndP.js', 'E'],
  ElOption: ['el-select-DfbR1ndP.js', 'a'],
  ElTag: ['el-select-DfbR1ndP.js', 'b'],
  ElTable: ['el-table-column-C9BPYNvZ.js', 'E'],
  ElTableColumn: ['el-table-column-C9BPYNvZ.js', 'a'],
  ElDialog: ['el-dialog-CgOVebwF.js', 'E'],
  ElLoadingDirective: ['el-dialog-CgOVebwF.js', 'v'],
  ElForm: ['el-form-item-CvvzSLKj.js', 'E'],
  ElFormItem: ['el-form-item-CvvzSLKj.js', 'a'],
}

export const bridges = {
  vue: Object.fromEntries(Object.entries(vueExports).map(([name, alias]) => [name, [main, alias]])),
  'vue-router': { useRoute: [main, 'e'], useRouter: [main, 'u'] },
  'element-plus': elementExports,
  'legacy-api': { api: [main, 'L'] },
  'legacy-page-header': { default: [helper, 'a'] },
}

export function bridgeSource(exports) {
  return Object.entries(exports).map(([name, [file, alias]]) =>
    `export { ${alias} as ${name} } from ${JSON.stringify(`/assets/${file}`)};`,
  ).join('\n')
}
