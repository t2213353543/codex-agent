"""Extract pinned 0.155.0 prompt fragments, with per-source SHA256 provenance."""
import hashlib
import json
from pathlib import Path
import re

ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / '.local/codex-source-0.155.0/codex-rs'


def main():
    templates, provenance = {}, {}

    def read(relative):
        path = SOURCE / relative
        raw = path.read_bytes()
        provenance[relative] = hashlib.sha256(raw).hexdigest()
        return raw.decode().replace('\r\n', '\n')

    for key, relative in {
        'permission_read_only': 'prompts/templates/permissions/sandbox_mode/read_only.md',
        'approval_never': 'prompts/templates/permissions/approval_policy/never.md',
        'collaboration_default': 'collaboration-mode-templates/templates/default.md',
        'collaboration_plan': 'collaboration-mode-templates/templates/plan.md',
    }.items():
        templates[key] = read(relative)
    for relative, constants in {
        'core/src/session/multi_agents.rs': {
            'multiagent_root': 'DEFAULT_MULTI_AGENT_V2_ROOT_AGENT_USAGE_HINT_TEXT',
            'multiagent_shared': 'DEFAULT_MULTI_AGENT_V2_SHARED_USAGE_HINT_TEXT',
            'multiagent_wait': 'DEFAULT_MULTI_AGENT_V2_WAIT_AGENT_USAGE_HINT_TEXT',
            'multiagent_model_overrides': 'DEFAULT_MULTI_AGENT_V2_MODEL_OVERRIDE_USAGE_HINT_TEXT',
        },
        'core/src/context/multi_agent_mode_instructions.rs': {
            'multiagent_explicit': 'EXPLICIT_REQUEST_ONLY_MULTI_AGENT_MODE_TEXT',
            'multiagent_proactive': 'PROACTIVE_MULTI_AGENT_MODE_TEXT',
        },
    }.items():
        text = read(relative)
        for key, name in constants.items():
            match = re.search(r'const ' + name + r': &str =\s*(r#"(.*?)"#|"((?:\\.|[^"\\])*)");', text, re.S)
            if not match:
                raise ValueError('missing pinned source constant: ' + name)
            templates[key] = match.group(2) if match.group(2) is not None else json.loads('"' + match.group(3) + '"')
    target = ROOT / 'internal/server/appserver_source_templates_0155.json'
    target.write_text(json.dumps({'source_version': '0.155.0', 'source_sha256': provenance, 'templates': templates}, ensure_ascii=False, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'output': str(target), 'templates': len(templates)}))


if __name__ == '__main__':
    main()
