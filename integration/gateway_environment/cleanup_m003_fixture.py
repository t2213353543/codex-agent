"""Remove only the completed 72594cd8d7 local synthetic fixture, preserving evidence."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import subprocess


ROOT = Path(__file__).resolve().parents[2]
RUN = "72594cd8d7"
PREFIX = "codex-m003-" + RUN
LABEL = "codex-agent.integration.m003"
CONTAINERS = [PREFIX + suffix for suffix in ("-old", "-pg", "-redis")]
NETWORK, VOLUME = PREFIX + "-net", PREFIX + "-pg-data"


def docker(*args):
    result = subprocess.run(["docker", *args], capture_output=True, text=True, timeout=60)
    if result.returncode:
        raise RuntimeError("Scoped Docker operation failed: " + args[0])
    return result.stdout.strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--execute", action="store_true")
    args = parser.parse_args()
    inspected = [json.loads(docker("inspect", name))[0] for name in CONTAINERS]
    for name, value in zip(CONTAINERS, inspected):
        assert value["Name"] == "/" + name and value["Config"]["Labels"].get(LABEL) == RUN
        assert set(value["NetworkSettings"]["Networks"]) == {NETWORK}
    ids = {value["Id"] for value in inspected}
    network = json.loads(docker("network", "inspect", NETWORK))[0]
    assert network["Name"] == NETWORK and network["Labels"].get(LABEL) == RUN
    assert set(network.get("Containers", {})) <= ids
    volume = json.loads(docker("volume", "inspect", VOLUME))[0]
    assert volume["Name"] == VOLUME
    mounts = [mount for mount in inspected[1]["Mounts"] if mount.get("Type") == "volume" and mount.get("Name") == VOLUME]
    assert len(mounts) == 1 and mounts[0]["Destination"] == "/var/lib/postgresql"
    references = docker("ps", "-aq", "--no-trunc", "--filter", "volume=" + VOLUME).splitlines()
    assert references == [inspected[1]["Id"]], "The volume must only be referenced by the labelled fixture PostgreSQL"
    report = {"schema": "codex-agent-local-fixture-cleanup-v1", "status": "verified_only", "fixture_id": RUN,
              "containers": [{"name": name, "id": value["Id"], "running_before": value["State"]["Running"], "label_verified": True} for name, value in zip(CONTAINERS, inspected)],
              "network": {"name": NETWORK, "id": network["Id"], "label_verified": True},
              "volume": {"name": VOLUME, "labels": volume.get("Labels"), "only_reference": references[0], "association_verified": True},
              "local_evidence_preserved": True, "remote_resources_touched": False, "other_projects_touched": False}
    if args.execute:
        for value in inspected:
            current = json.loads(docker("inspect", value["Id"]))[0]
            assert current["Config"]["Labels"].get(LABEL) == RUN and current["Name"] == value["Name"]
            if current["State"]["Running"]:
                docker("stop", "--time", "20", value["Id"])
            docker("rm", value["Id"])
        assert not docker("ps", "-aq", "--filter", "volume=" + VOLUME)
        docker("volume", "rm", VOLUME)
        assert not json.loads(docker("network", "inspect", NETWORK))[0].get("Containers")
        docker("network", "rm", NETWORK)
        report["status"] = "completed"
        report["remaining_labelled_containers"] = docker("ps", "-aq", "--filter", "label=" + LABEL + "=" + RUN).splitlines()
        assert not report["remaining_labelled_containers"]
    report["completed_at"] = datetime.now(timezone.utc).isoformat()
    output = ROOT / ".local/migration-003-local/20260918T024916-72594cd8d7" / ("cleanup-report-" + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S") + ".json")
    with output.open("x", encoding="utf-8") as handle:
        json.dump(report, handle, indent=2)
        handle.write("\n")
    print(json.dumps({"status": report["status"], "report": str(output)}))


if __name__ == "__main__":
    main()
