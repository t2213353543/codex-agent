import { mergeConfig } from 'vite'
import config from '../vite.config.js'

// Keep lifecycle assertions deterministic while other agents edit unrelated
// frontend files. This local mock server never forwards an API request.
export default mergeConfig(config, { server: { watch: null } })
