#!/usr/bin/env python3
"""Prepare an isolated PR #281 SQLite deployment on the 168 test host."""

import json
import os
import secrets
import sys
from pathlib import Path

import yaml


def main() -> None:
    root = Path(sys.argv[1]).resolve(strict=True)
    if root.name != "pr281-sqlite" or root.parent != Path("/home/hackers365/shijingbo/git"):
        raise SystemExit("unexpected deployment directory")

    secrets_dir = root / "deployment"
    secrets_dir.mkdir(mode=0o700, exist_ok=True)
    credentials_path = secrets_dir / "credentials.json"
    if credentials_path.exists():
        raise SystemExit("credentials already exist; refusing to rotate them")

    old_umask = os.umask(0o077)
    try:
        credentials = {
            "admin_username": "pr281admin",
            "admin_email": "pr281@example.invalid",
            "admin_password": secrets.token_urlsafe(24),
            "jwt_secret": secrets.token_urlsafe(48),
            "internal_auth_token": secrets.token_urlsafe(48),
            "endpoint_auth_token": secrets.token_urlsafe(48),
        }
        with credentials_path.open("x", encoding="utf-8") as stream:
            json.dump(credentials, stream, indent=2)
            stream.write("\n")

        backend_path = root / "manager/backend/config/config.json"
        backend = json.loads(backend_path.read_text(encoding="utf-8"))
        backend["server"] = {"host": "127.0.0.1", "port": "18180", "mode": "release"}
        backend["database"] = {
            "type": "sqlite",
            "sqlite": {"file_path": str(root / "manager/backend/data/pr281.db")},
        }
        backend["jwt"]["secret"] = credentials["jwt_secret"]
        backend["internal_auth_token"] = credentials["internal_auth_token"]
        backend["endpoint_auth_token"] = credentials["endpoint_auth_token"]
        backend["speaker_service"]["url"] = "http://127.0.0.1:19100"
        backend["storage"]["speaker_audio_path"] = str(root / "manager/backend/storage/speakers")
        backend["history"]["audio_base_path"] = str(root / "manager/backend/data/chat_history/audio")
        backend_path.write_text(json.dumps(backend, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        backend_path.chmod(0o600)

        server_path = root / "config/config.yaml"
        server = yaml.safe_load(server_path.read_text(encoding="utf-8"))
        server["manager"]["backend_url"] = "http://127.0.0.1:18180"
        server["manager"]["auth_token"] = credentials["internal_auth_token"]
        server["manager"]["endpoint_auth_token"] = credentials["endpoint_auth_token"]
        server["log"].update({"path": "logs/", "file": "server.log", "level": "info", "stdout": True})
        server["redis"].update({"db": 15, "key_prefix": "pr281_sqlite"})
        server["websocket"].update({"host": "0.0.0.0", "port": 18989})
        server["mqtt"]["enable"] = False
        server["mqtt_server"]["enable"] = False
        server["udp"].update({"external_host": "192.168.209.168", "external_port": 18990, "listen_port": 18990})
        server["voice_identify"]["enable"] = False
        server["mcp"]["global"]["enabled"] = False
        server["ota"]["test"]["websocket"]["url"] = "ws://192.168.209.168:18989/xiaozhi/v1/"
        server["ota"]["external"]["websocket"]["url"] = "ws://192.168.209.168:18989/xiaozhi/v1/"
        server["ota"]["test"]["mqtt"]["enable"] = False
        server["ota"]["external"]["mqtt"]["enable"] = False
        server_path.write_text(yaml.safe_dump(server, allow_unicode=True, sort_keys=False), encoding="utf-8")
        server_path.chmod(0o600)

        (root / "logs").mkdir(exist_ok=True)
        print("isolated SQLite configuration ready")
    finally:
        os.umask(old_umask)


if __name__ == "__main__":
    main()
