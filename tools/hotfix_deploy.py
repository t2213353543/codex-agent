#!/usr/bin/env python3
"""Quick hotfix deployment for configuration changes without full release cycle."""
import hashlib
import json
import sys
import tarfile
import tempfile
from pathlib import Path
import subprocess

sys.path.insert(0, 'D:/ChatGPT/CodexHome/skills/sub2api-production/scripts')
from sub2api_ssh import connect

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / 'codex-agent-linux-amd64'
FRONTEND = ROOT / 'frontend/dist'
REMOTE_ROOT = '/opt/codex-agent'

def sha256(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        while chunk := f.read(1024 * 1024):
            h.update(chunk)
    return h.hexdigest()

def main():
    # Verify local artifacts
    if not BINARY.exists():
        print(f"❌ Binary not found: {BINARY}")
        return 1
    if not (FRONTEND / 'index.html').exists():
        print(f"❌ Frontend not built: {FRONTEND}")
        return 1

    binary_sha = sha256(BINARY)
    print(f"✓ Binary SHA256: {binary_sha}")
    print(f"✓ Binary size: {BINARY.stat().st_size:,} bytes")

    # Create release package
    with tempfile.TemporaryDirectory() as tmpdir:
        tmpdir = Path(tmpdir)
        package = tmpdir / 'hotfix.tar.gz'

        print("\n📦 Creating package...")
        with tarfile.open(package, 'w:gz') as tar:
            tar.add(BINARY, arcname='codex-agent')
            tar.add(FRONTEND, arcname='frontend/dist')

        print(f"✓ Package created: {package.stat().st_size:,} bytes")

        # Upload and deploy
        print("\n🚀 Deploying to production...")
        ssh = connect('master')

        try:
            # Upload package
            print("  Uploading package...")
            sftp = ssh.open_sftp()
            remote_package = '/tmp/codex-hotfix.tar.gz'
            sftp.put(str(package), remote_package)
            sftp.close()
            print(f"  ✓ Uploaded to {remote_package}")

            # Execute deployment script
            deploy_script = f'''#!/bin/bash
set -euo pipefail

RELEASE_ID="{binary_sha}"
REMOTE_ROOT="{REMOTE_ROOT}"
PACKAGE="{remote_package}"

echo "Creating release directory..."
mkdir -p "$REMOTE_ROOT/releases/$RELEASE_ID"
cd "$REMOTE_ROOT/releases/$RELEASE_ID"

echo "Extracting package..."
tar xzf "$PACKAGE"
chmod +x codex-agent

echo "Stopping service..."
systemctl stop codex-agent.service

echo "Updating symlinks..."
ln -sf "$REMOTE_ROOT/releases/$RELEASE_ID/frontend/dist" "$REMOTE_ROOT/web-current"

echo "Updating environment..."
sed -i 's|^ExecStart=.*|ExecStart=/opt/codex-agent/releases/'"$RELEASE_ID"'/codex-agent --listen 0.0.0.0:18879 --web /opt/codex-agent/web-current --db-root /opt/codex-agent-db|' /etc/systemd/system/codex-agent.service

echo "Reloading systemd..."
systemctl daemon-reload

echo "Starting service..."
systemctl start codex-agent.service

echo "Waiting for health check..."
sleep 3
curl -f http://127.0.0.1:18879/health/ready

echo "Cleaning up..."
rm "$PACKAGE"

echo "✓ Deployment complete!"
'''

            stdin, stdout, stderr = ssh.exec_command(f'bash -s', timeout=60)
            stdin.write(deploy_script)
            stdin.channel.shutdown_write()

            output = stdout.read().decode()
            errors = stderr.read().decode()
            exit_code = stdout.channel.recv_exit_status()

            print("\n📋 Deployment output:")
            print(output)

            if errors:
                print("\n⚠️  Stderr:")
                print(errors)

            if exit_code != 0:
                print(f"\n❌ Deployment failed with exit code {exit_code}")
                return 1

            print("\n✅ Deployment successful!")
            print(f"   Release ID: {binary_sha}")

        finally:
            ssh.close()

    return 0

if __name__ == '__main__':
    sys.exit(main())
