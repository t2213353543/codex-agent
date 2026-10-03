#!/usr/bin/env python3
"""Sample the two authorized 28765 children without auth/inference requests.

python tools/oauth_child_lifetime_snapshot.py --remote
python tools/oauth_child_lifetime_snapshot.py --remote --selector-file /root/loophub-28765/ops/oauth-experiment/private-selectors.json --server-jsonl /root/loophub-28765/ops/oauth-experiment/lifetime.jsonl

The optional private selector file stays on the server and contains a JSON list
of {owner_ref, child_ref, email}. It lets the observer follow an original child
after rotation clears owner_id, using its indexed email primary key. Without it,
only the original children still belonging to these owners can be observed.
Only anonymous status/timestamps and reason categories/hashes leave the server.
No timer installation, application import, credential read, or database write.
"""
import argparse
import json
import os
from pathlib import Path
import shlex
import stat
import subprocess
import sys

TARGETS = (
    {"owner_ref": "36802847ceb1c167305f48d627d32096", "child_ref": "7ffd2aeff6aa1aff143bb78b70f7c554"},
    {"owner_ref": "fa000e9a1ec463e1b4eac159f48574e4", "child_ref": "2d3524dbef4877afde33d7d5a0ad0edf"},
)

CONTAINER_SCRIPT = r'''
import hashlib,json,math,os,sys
from datetime import datetime,timezone
import psycopg
from psycopg.rows import dict_row

request=json.load(sys.stdin)
targets=request['targets']
selectors=request.get('selectors') or []
aliases={(v['owner_ref'],v['child_ref']) for v in targets}
for item in selectors:
    if (item.get('owner_ref'),item.get('child_ref')) not in aliases:
        raise ValueError('selector_scope_mismatch')
    if hashlib.md5(item['email'].encode()).hexdigest()!=item['child_ref']:
        raise ValueError('selector_identity_mismatch')
if selectors and len(selectors)!=len(targets):
    raise ValueError('selector_count_mismatch')

def enum(value,allowed):
    value=str(value or '').lower()
    return value if value in allowed else ('unknown' if value else None)
def timestamp(value):
    if value is None:return None
    number=float(value)
    return number if math.isfinite(number) else None
def reason(value):
    value=str(value or '')
    if not value:return {'category':None,'sha256':None}
    lower=value.lower()
    groups=(('usage_policy',('usage policy','policy_violation')),
      ('account_deactivated',('deactivated','account_deleted','dead')),
      ('quota',('quota','exhaust','rate_limit','rate limit','额度')),
      ('authentication',('401','invalid_grant','refresh_token','oauth','unauthorized')),
      ('forbidden',('403','forbidden')),
      ('membership',('removed','seat','membership','workspace','席位')),
      ('network',('timeout','timed out','network','proxy','connect')))
    category=next((name for name,needles in groups if any(s in lower for s in needles)),'other')
    return {'category':category,'sha256':hashlib.sha256(value.encode()).hexdigest()}

columns="""
c.email,c.owner_id,c.status,c.seat_type,c.assigned_at,c.removed_at,c.dead_at,
c.last_checked_at,c.updated_at,c.next_eligible_at,c.primary_reset_at,c.secondary_reset_at,
c.oauth_reauth_status,c.oauth_reauth_next_at,c.oauth_reauth_updated_at,
c.oauth_reauth_failures,c.last_error,c.oauth_reauth_error,c.dead_reason,
r.email IS NOT NULL registered_present,r.account_status,r.status_at,r.status_reason,
r.deleted_at,r.created_at registered_created_at
"""
connection=psycopg.connect(os.environ['DATABASE_URL'],autocommit=True,
    options='-c default_transaction_read_only=on -c statement_timeout=10000',row_factory=dict_row)
output=[]
with connection,connection.transaction():
    # One consistent, read-only transaction; only two indexed children are read
    # when a private selector file is supplied. No auth/application imports.
    connection.execute('SET TRANSACTION READ ONLY')
    for target in targets:
        owners=connection.execute('SELECT id,status FROM team_rotation_owners WHERE md5(id)=%s',(target['owner_ref'],)).fetchall()
        if len(owners)>1:raise ValueError('ambiguous_owner_reference')
        owner=owners[0] if owners else None
        fixed=next((s for s in selectors if s['child_ref']==target['child_ref']),None)
        if fixed:
            rows=connection.execute('SELECT '+columns+' FROM team_rotation_children c LEFT JOIN registered r ON r.email=c.email WHERE c.email=%s',(fixed['email'],)).fetchall()
        elif owner:
            rows=connection.execute('SELECT '+columns+' FROM team_rotation_children c LEFT JOIN registered r ON r.email=c.email WHERE c.owner_id=%s AND md5(c.email)=%s',(owner['id'],target['child_ref'])).fetchall()
        else:rows=[]
        if len(rows)>1:raise ValueError('ambiguous_child_reference')
        observed={'owner_ref':target['owner_ref'],'child_ref':target['child_ref'],
          'owner_present':bool(owner),'owner_status':enum(owner['status'],{'active','disabled'}) if owner else None,
          'tracked_by_primary_key':bool(fixed),'child_present':bool(rows)}
        if not rows:
            observed['observation']='original_child_missing' if fixed else 'original_child_not_in_owner_pool'
            output.append(observed);continue
        row=rows[0]
        if hashlib.md5(row['email'].encode()).hexdigest()!=target['child_ref']:
            raise ValueError('observed_identity_mismatch')
        observed.update(observation='observed',same_owner=bool(owner and row['owner_id']==owner['id']),
          current_owner_ref=hashlib.md5(row['owner_id'].encode()).hexdigest() if row['owner_id'] else None,
          status=enum(row['status'],{'active','pending','available','removed','removal_pending','dead','exhausted','error'}),
          seat_type=enum(row['seat_type'],{'standard','advanced','unknown'}),
          registered_present=bool(row['registered_present']),
          registered_status=enum(row['account_status'],{'active','dead','oauth_error','disabled','unknown'}),
          oauth_reauth_status=enum(row['oauth_reauth_status'],{'success','pending','running','failed','error','skipped','retry','waiting'}),
          oauth_reauth_failures=int(row['oauth_reauth_failures'] or 0),
          rotation_reason=reason(row['last_error']),oauth_reason=reason(row['oauth_reauth_error']),
          dead_reason=reason(row['dead_reason']),registered_reason=reason(row['status_reason']))
        for field in ('assigned_at','removed_at','dead_at','last_checked_at','updated_at','next_eligible_at',
          'primary_reset_at','secondary_reset_at','oauth_reauth_next_at','oauth_reauth_updated_at',
          'status_at','deleted_at','registered_created_at'):
            observed[field]=timestamp(row[field])
        output.append(observed)
print(json.dumps({'captured_at':datetime.now(timezone.utc).isoformat(),'source':'28765_readonly_database',
    'targets':output,'is_lifecycle_tracking':bool(selectors),'upstream_requests':0,
    'database_writes':0,'auth_material_selected':False,'query_scope':'two_primary_keys' if selectors else 'two_owner_index_ranges'}))
'''


def private_selectors(path):
    if path is None:
        return []
    descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "r", encoding="utf-8") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) & 0o077:
            raise ValueError("selector_file_must_be_private_regular_file")
        value = json.load(stream)
    if not isinstance(value, list) or len(value) != len(TARGETS):
        raise ValueError("invalid_selector_file")
    allowed = {(v["owner_ref"], v["child_ref"]) for v in TARGETS}
    if {(v.get("owner_ref"), v.get("child_ref")) for v in value} != allowed:
        raise ValueError("selector_scope_mismatch")
    return [{key: row[key] for key in ("owner_ref", "child_ref", "email")} for row in value]


def append_private(path, report):
    import fcntl
    destination = Path(path)
    if not destination.is_absolute():
        raise ValueError("server_jsonl_requires_absolute_path")
    destination.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor = os.open(destination, os.O_WRONLY | os.O_APPEND | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    with os.fdopen(descriptor, "a", encoding="utf-8") as stream:
        if not stat.S_ISREG(os.fstat(stream.fileno()).st_mode):
            raise ValueError("server_jsonl_requires_regular_file")
        fcntl.flock(stream, fcntl.LOCK_EX)
        os.fchmod(stream.fileno(), 0o600)
        stream.write(json.dumps(report, separators=(",", ":")) + "\n")
        stream.flush()


def server_snapshot(args):
    payload = {"targets": TARGETS, "selectors": private_selectors(args.selector_file)}
    process = subprocess.run(["docker", "exec", "-i", "loophub-multitenant-app", "python", "-B", "-c", CONTAINER_SCRIPT],
        input=json.dumps(payload), capture_output=True, text=True, timeout=35)
    if process.returncode:
        # Database errors and tracebacks may contain DSNs/parameters. Keep them
        # out of both the journal and stdout, including on unsuccessful probes.
        raise RuntimeError("container_readonly_snapshot_failed")
    report = json.loads(process.stdout)
    if args.server_jsonl:
        append_private(args.server_jsonl, report)
    return report


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--remote", action="store_true")
    mode.add_argument("--server-once", action="store_true")
    parser.add_argument("--selector-file", help="Existing server-only private selector JSON; never downloaded")
    parser.add_argument("--server-jsonl", help="Append an anonymous JSONL record on server, mode 0600")
    args = parser.parse_args()
    if args.server_once:
        report = server_snapshot(args)
    else:
        from integration_env import connect
        client = connect()
        try:
            command = ["python3", "-B", "-", "--server-once"]
            if args.selector_file:
                command.extend(["--selector-file", args.selector_file])
            if args.server_jsonl:
                command.extend(["--server-jsonl", args.server_jsonl])
            stdin, stdout, stderr = client.exec_command(shlex.join(command), timeout=45)
            stdin.write(Path(__file__).read_text(encoding="utf-8"))
            stdin.channel.shutdown_write()
            raw = stdout.read()
            stderr.read()  # never echo a remote traceback
            if stdout.channel.recv_exit_status():
                raise RuntimeError("remote_readonly_snapshot_failed")
            report = json.loads(raw)
        finally:
            client.close()
    print(json.dumps(report, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"snapshot_error": type(error).__name__, "message": "Read-only snapshot failed; no credentials exported"}), file=sys.stderr)
        sys.exit(1)
