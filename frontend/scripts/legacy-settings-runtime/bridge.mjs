// Settings shares the exact Vue/Element runtime of the restored 19:33 shell.
// Aliases are verified against pinned immutable artifacts by the build script.
import {
  bridges as logsBridges, elementExports as logsElementExports,
  legacyRelease, legacyManifestSHA, legacyMain, bridgeSource,
} from '../legacy-logs-runtime/bridge.mjs'

export { legacyRelease, legacyManifestSHA, legacyMain, bridgeSource }
export const legacySettings = 'Settings-CU7oOkU4.js'
export const legacySettingsScope = '2da67541'
export const logsManifestSHA = '9774a1e5360887ed784ebfe86ea6bf4c4ef018662f292c782aeadb25de7ed467'
export const elementExports = {
  ...logsElementExports,
  ElSwitch: ['el-switch-C8o5Pt5s.js', 'E'],
  ElMessageBox: [legacyMain, 'Z'],
}
export const bridges = {
  ...logsBridges,
  'element-plus': elementExports,
  'legacy-settings-management': {
    useManagementRead: ['management-DbTmSyZn.js', 'u'],
    managementRotationRequestId: ['management-DbTmSyZn.js', 'a'],
  },
}
