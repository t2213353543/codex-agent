// Shared runtime aliases are tied to the immutable restored release.
import { bridges as previous, elementExports as previousElements, legacyRelease, legacyManifestSHA, legacyMain, bridgeSource } from '../legacy-settings-runtime/bridge.mjs'
export { legacyRelease, legacyManifestSHA, legacyMain, bridgeSource }
export const legacyAccounts = 'Accounts-CO5sQjFD.js'
export const legacyLogs = 'Logs-ZPqLUryO.js'
export const settingsManifestSHA = '81a52df422c809c6f375e29eb52261ee47ff32d004435b897166748666c9ce59'
// Expose existing bindings from a byte-identical copy plus this export only.
// Import-map redirects every old/new consumer to that single module instance.
export const extraMainExports = {
  __managementRequest: 'Fe',
  __managementClearSession: 'i0',
  __managementAuthGeneration: 'Lo',
  __managementDropdown: '$8',
  __managementDropdownItem: 'k8',
  __managementDropdownMenu: 'L8',
}
export const elementExports = {
  ...previousElements,
  ElInputNumber: ['el-input-number-Bl1QIfpX.js', 'E'],
  ElCheckbox: ['el-table-column-C9BPYNvZ.js', 'b'],
  ElCheckboxGroup: ['el-table-column-C9BPYNvZ.js', 'c'],
  ElTooltip: [legacyMain, 'aD'],
  ElDropdown: [legacyMain, '__managementDropdown'],
  ElDropdownItem: [legacyMain, '__managementDropdownItem'],
  ElDropdownMenu: [legacyMain, '__managementDropdownMenu'],
  ElRow: ['echarts-qaEpvw0Y.js', 'E'],
  ElCol: ['echarts-qaEpvw0Y.js', 'a'],
}
export const bridges = {
  ...previous,
  vue: { ...previous.vue, toRef: [legacyMain, 'X'] },
  'element-plus': elementExports,
  'legacy-api': { api: [legacyMain, 'L'], request: [legacyMain, '__managementRequest'], clearSession: [legacyMain, '__managementClearSession'], authGeneration: [legacyMain, '__managementAuthGeneration'] },
  echarts: { init: ['echarts-qaEpvw0Y.js', 'i'] },
  'legacy-echarts': { init: ['echarts-qaEpvw0Y.js', 'i'] },
}
