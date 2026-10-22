#!/usr/bin/env python3
"""Rotate only the isolated PR281 service tokens before first use."""

import json
import secrets
import sys
from pathlib import Path

import yaml


def main() -> None:
    root = Path(sys.argv[1]).resolve(strict=True)
    if root.name != "pr281-sqlite" or root.parent != Path("/home/hackers365/shijingbo/git"):
        raise SystemExit("unexpected deployment directory")

    credentials_path = root / "deployment/credentials.json"
    credentials = json.loads(credentials_path.read_text(encoding="utf-8"))
    for key in ("jwt_secret", "internal_auth_token", "endpoint_auth_token"):
        credentials[key] = secrets.token_urlsafe(48)

    backend_path = root / "manager/backend/config/config.json"
    backend = json.loads(backend_path.read_text(encoding="utf-8"))
    backend["jwt"]["secret"] = credentials["jwt_secret"]
    backend["internal_auth_token"] = credentials["internal_auth_token"]
    backend["endpoint_auth_token"] = credentials["endpoint_auth_token"]
    backend_path.write_text(json.dumps(backend, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    backend_path.chmod(0o600)

    server_path = root / "config/config.yaml"
    server = yaml.safe_load(server_path.read_text(encoding="utf-8"))
    server["manager"]["auth_token"] = credentials["internal_auth_token"]
    server["manager"]["endpoint_auth_token"] = credentials["endpoint_auth_token"]
    server_path.write_text(yaml.safe_dump(server, allow_unicode=True, sort_keys=False), encoding="utf-8")
    server_path.chmod(0o600)

    credentials_path.write_text(json.dumps(credentials, indent=2) + "\n", encoding="utf-8")
    credentials_path.chmod(0o600)
    print("isolated service tokens rotated")


if __name__ == "__main__":
    main()
