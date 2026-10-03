export function validModelName(value) { return typeof value === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$/.test(value) }
export function gatewayBaseURL(origin) {
  const url = new URL(origin)
  if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password) throw new Error('无效服务地址')
  return `${url.origin}/v1`
}
export function clientConfiguration(origin, model) {
  if (!validModelName(model)) throw new Error('请输入有效的模型名称')
  return `model_provider = "powerpoint_gateway"\nmodel = "${model}"\nmodel_reasoning_effort = "xhigh"\n\n[model_providers.powerpoint_gateway]\nname = "OpenAI"\nbase_url = "${gatewayBaseURL(origin)}"\nenv_key = "CODEX_AGENT_KEY"\nwire_api = "responses"\nsupports_websockets = true\n`
}
export function clientKeyCommand(shell) {
  return shell === 'powershell' ? "$env:CODEX_AGENT_KEY = 'YOUR_GATEWAY_KEY'" : "export CODEX_AGENT_KEY='YOUR_GATEWAY_KEY'"
}
