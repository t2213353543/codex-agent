"""Provision only the fixed synthetic deployment rehearsal database/config."""
import base64
import json
from pathlib import Path
import secrets
import shlex
from integration_env import connect,run,write_remote,ROOT

REMOTE='/opt/codex-agent-integration/rehearsal'
if __name__=='__main__':
    client=connect()
    try:
        run(client,'test -f /opt/codex-agent-integration/.codex-agent-integration; test ! -d '+REMOTE+' || test -f '+REMOTE+'/.synthetic-rehearsal')
        exists=run(client,'test -f '+REMOTE+'/config/production.env && echo exists || true').strip()
        if exists:print('Existing rehearsal secrets preserved')
        else:
            config=json.loads((ROOT/'.local/integration/environment.json').read_text())
            migrator,runtime=secrets.token_hex(24),secrets.token_hex(24)
            sql=f"CREATE ROLE codex_agent_rehearsal_migrator LOGIN PASSWORD '{migrator}' NOSUPERUSER NOCREATEDB NOCREATEROLE;\nCREATE ROLE codex_agent_rehearsal_runtime LOGIN PASSWORD '{runtime}' NOSUPERUSER NOCREATEDB NOCREATEROLE CONNECTION LIMIT 16;\nCREATE DATABASE codex_agent_rehearsal OWNER codex_agent_rehearsal_migrator;\nREVOKE CONNECT ON DATABASE codex_agent_rehearsal FROM PUBLIC;\nGRANT CONNECT ON DATABASE codex_agent_rehearsal TO codex_agent_rehearsal_runtime;\n\\connect codex_agent_rehearsal\nREVOKE CREATE ON SCHEMA public FROM PUBLIC;\nGRANT USAGE ON SCHEMA public TO codex_agent_rehearsal_runtime;\nALTER DEFAULT PRIVILEGES FOR ROLE codex_agent_rehearsal_migrator IN SCHEMA public GRANT SELECT,INSERT,UPDATE,DELETE ON TABLES TO codex_agent_rehearsal_runtime;\nALTER DEFAULT PRIVILEGES FOR ROLE codex_agent_rehearsal_migrator IN SCHEMA public GRANT USAGE,SELECT ON SEQUENCES TO codex_agent_rehearsal_runtime;\n"
            run(client,'install -d -m 700 '+REMOTE+' '+REMOTE+'/config')
            write_remote(client,REMOTE+'/.synthetic-rehearsal','codex-agent-synthetic-rehearsal\n')
            write_remote(client,REMOTE+'/config/init.sql',sql)
            run(client,'docker exec -i codex-agent-integration-postgres psql -U postgres -v ON_ERROR_STOP=1 < '+REMOTE+'/config/init.sql')
            env={'DATABASE_URL':f'postgres://codex_agent_rehearsal_runtime:{runtime}@127.0.0.1:25432/codex_agent_rehearsal?sslmode=disable',
                'REDIS_URL':config['REDIS_URL'],'REDIS_KEY_PREFIX':'codex-agent:integration:rehearsal:',
                'CREDENTIAL_ENCRYPTION_KEY':base64.b64encode(secrets.token_bytes(32)).decode(),'CREDENTIAL_ENCRYPTION_KEY_ID':'rehearsal-v1',
                'ADMIN_SESSION_TTL':'3600','ADMIN_COOKIE_SECURE':'false','STATS_TIMEZONE':'Asia/Shanghai','PG_MAX_CONNS':'8'}
            write_remote(client,REMOTE+'/config/production.env',''.join(k+'='+v+'\n' for k,v in env.items()))
            write_remote(client,REMOTE+'/config/migration.env',f'DATABASE_URL=postgres://codex_agent_rehearsal_migrator:{migrator}@127.0.0.1:25432/codex_agent_rehearsal?sslmode=disable\n')
            print('Synthetic rehearsal database/config ready; production service unchanged')
    finally:client.close()
