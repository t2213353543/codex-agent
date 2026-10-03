#!/usr/bin/env python3
"""Read-only source audit for removal of hosted official production runtimes.

python tools/audit_runtime_removal.py
This verifies current source gates, not production deployment or successful
model content. It never edits backend code or makes inference requests.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import sys


def without_go_comments(source):
    # Sufficient for constructor-call checks; exact files/hash remain evidence.
    return re.sub(r"(?m)^\s*//.*$", "", re.sub(r"/\*.*?\*/", "", source, flags=re.S))


def audit(root):
    paths = ["internal/server/main.go", "internal/server/admin.go", "internal/server/gateway.go",
        "internal/server/gateway_inference_mode.go", "frontend/index.html", "frontend/src/router/index.js",
        "frontend/src/components/PowerPointService.vue", "internal/bps/request.go"]
    sources = {path: (root / path).read_text(encoding="utf-8") for path in paths}
    main = without_go_comments(sources[paths[0]])
    options = sources[paths[3]].split("func inferenceModeOptions()", 1)[1].split("\nfunc ", 1)[0]
    gateway = sources[paths[2]]
    route_match = re.search(r'if strings\.HasPrefix\(r\.URL\.Path, "/v1/codex/"\).*?\n\s*}', gateway, re.S)
    route = route_match.group(0) if route_match else ""
    admin = sources[paths[1]]
    checks = {
        "main_does_not_construct_official_runtime": not re.search(r"\bnewOfficialRuntime\s*\(", main),
        "main_does_not_construct_app_server_cli": not re.search(r"\bnewAppServerCLI\s*\(", main),
        "main_keeps_powerpoint_runtime": "newBPSRuntime(" in main,
        "management_rejects_removed_mode": bool(re.search(r'if \*input\.InferenceMode == inferenceAppServer\s*\{\s*apiError\(w, http\.StatusBadRequest, "requested inference mode has been removed"\)', admin)),
        "management_offers_no_app_server": "inferenceAppServer" not in options,
        "management_offers_no_hosted_runtime": not re.search(r'"hosted_runtime"\s*:\s*true', options),
        "codex_hosted_endpoints_unavailable_without_runtime": "p.Official == nil" in route and "http.StatusNotFound" in route,
        "frontend_html_title": "<title>高级反代系统</title>" in sources["frontend/index.html"],
        "frontend_router_title": bool(re.search(r"document\.title\s*=\s*['\"]高级反代系统['\"]", sources["frontend/src/router/index.js"])),
        "frontend_no_hardcoded_appserver_option": "official_app_server" not in sources["frontend/src/components/PowerPointService.vue"],
        "unfinished_image_resolver_removed": not (root / "internal/server/gateway_official_images.go").exists(),
    }
    error_copy = []
    sanitizer_path=root/"internal/bps/errors.go"
    sanitizer= sanitizer_path.read_text(encoding="utf-8") if sanitizer_path.exists() else ""
    parameter_errors_sanitized='publicerrors.Message(' in sanitizer
    for path in (root / "internal/bps").glob("*.go"):
        if path.name.endswith("_test.go"):
            continue
        for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
            if "PowerPoint" in line and any(marker in line for marker in ("unsupportedParameter(", "errors.New(", "fmt.Errorf(")):
                error_copy.append({"file": str(path.relative_to(root)), "line": number, "source": line.strip(),
                    "public_error_sanitizer_applies": "unsupportedParameter(" in line and parameter_errors_sanitized})
    return {"schema": "hosted-runtime-source-removal-v1", "checks": checks,
        "source_gates_passed": all(checks.values()), "remaining_powerpoint_error_copy": error_copy,
        "production_deployment_proven": False, "model_output_inspected_or_rewritten": False,
        "source_sha256": {path: hashlib.sha256((root / path).read_bytes()).hexdigest() for path in paths}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    result = audit(args.root)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result["source_gates_passed"] else 1


if __name__ == "__main__": sys.exit(main())
