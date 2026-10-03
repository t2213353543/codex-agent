"""Record database privilege, Redis ACL and container resource isolation."""
import json
from pathlib import Path
import socket
import sys
import urllib.parse
from integration_env import connect,run,ROOT

sys.path.insert(0,str(ROOT/'.local/performance-python'))
import psycopg


def main():
    config=json.loads((ROOT/'.local/integration/environment.json').read_text())
    assert config['purpose']=='codex-agent-integration' and config['synthetic_data'] is True
    with psycopg.connect(config['DATABASE_URL']) as connection:
        row=connection.execute('SELECT current_database(),current_user,rolsuper,rolcreatedb,rolcreaterole,rolreplication FROM pg_roles WHERE rolname=current_user').fetchone()
        assert row[:2]==('codex_agent_integration','codex_agent_test') and not any(row[2:])
        settings=dict(connection.execute("SELECT name,setting FROM pg_settings WHERE name IN ('max_connections','statement_timeout','lock_timeout','shared_buffers')").fetchall())
    target=urllib.parse.urlsplit(config['REDIS_URL'])
    with socket.create_connection((target.hostname,target.port),timeout=5) as redis:
        redis.settimeout(5)
        stream=redis.makefile('rb')
        def command(*parts):
            raw=('*'+str(len(parts))+'\r\n'+''.join('$'+str(len(x.encode()))+'\r\n'+x+'\r\n' for x in parts)).encode()
            redis.sendall(raw)
            return stream.readline().decode().strip()
        assert command('AUTH',target.username,target.password)=='+OK'
        assert command('PING')=='+PONG'
        assert command('SET','codex-agent:integration:isolation-proof','synthetic','EX','30')=='+OK'
        assert command('SET','unrelated:isolation-proof','synthetic','EX','30').startswith('-NOPERM')
        assert command('FLUSHALL').startswith('-NOPERM')
        assert command('DEL','codex-agent:integration:isolation-proof')==':1'
    client=connect()
    try:
        containers=json.loads(run(client,"docker inspect codex-agent-integration-postgres codex-agent-integration-redis --format '{{json .}}' | python3 -c 'import sys,json; rows=[json.loads(x) for x in sys.stdin]; print(json.dumps([{\"name\":r[\"Name\"],\"image\":r[\"Image\"],\"memory\":r[\"HostConfig\"][\"Memory\"],\"nano_cpus\":r[\"HostConfig\"][\"NanoCpus\"],\"pids\":r[\"HostConfig\"][\"PidsLimit\"],\"ports\":r[\"HostConfig\"][\"PortBindings\"]} for r in rows]))'"))
        assert all(r['memory']>0 and r['nano_cpus']>0 and r['pids']>0 for r in containers)
        assert all(p['HostIp']=='127.0.0.1' for r in containers for binding in r['ports'].values() for p in binding)
    finally:client.close()
    report={'checks':[{'id':'database.isolation','status':'pass','detail':'Dedicated PG/Redis containers; non-superuser non-createdb non-createrole non-replication role; Redis permits only integration prefix and denies FLUSHALL; loopback ports and bounded CPU/memory/PIDs'}],
        'database':{'name':row[0],'user':row[1],'settings':settings},'redis_namespace':config['REDIS_KEY_PREFIX'],'containers':containers}
    path=ROOT/'.local/integration/isolation.json';path.write_text(json.dumps(report,indent=2))
    print(path)


if __name__=='__main__':main()
