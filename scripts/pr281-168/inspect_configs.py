#!/usr/bin/env python3
"""Print safe metadata for pro and isolated PR281 voice configs."""

import json
import sqlite3
import sys
from pathlib import Path
from urllib.parse import urlparse


PRO_ROOT = Path("/home/hackers365/shijingbo/git/xiaozhi-esp32-server-golang-pro/manager/backend")
TEST_DB = Path("/home/hackers365/shijingbo/git/pr281-sqlite/manager/backend/data/pr281.db")
TYPES = ("vad", "asr", "llm", "tts")
SAFE_FIELDS = ("model", "model_name", "voice", "format", "stream", "sample_rate", "mode", "language")


def inspect(label: str, path: Path) -> None:
    print(label, "database_exists=", path.exists())
    if not path.exists():
        return
    db = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
    db.row_factory = sqlite3.Row
    columns = [column[1] for column in db.execute("PRAGMA table_info(configs)")]
    print(label, "columns=", columns)
    if "configs" not in [row[0] for row in db.execute("SELECT name FROM sqlite_master WHERE type='table'")]:
        return
    rows = db.execute(
        "SELECT type, config_id, provider, name, enabled, is_default, json_data "
        "FROM configs WHERE type IN (?, ?, ?, ?) ORDER BY type, is_default DESC, id",
        TYPES,
    )
    for row in rows:
        if len(sys.argv) > 1 and sys.argv[1] == "--defaults" and not row["is_default"]:
            continue
        data = json.loads(row["json_data"] or "{}")
        safe = {key: data[key] for key in SAFE_FIELDS if key in data}
        if row["is_default"]:
            if "model_path" in data:
                safe["model_path"] = data["model_path"]
            safe["endpoint_hosts"] = {
                key: urlparse(data[key]).hostname
                for key in ("ws_url", "base_url", "api_url")
                if isinstance(data.get(key), str)
            }
            safe["credential_present"] = bool(data.get("api_keys") or data.get("api_key"))
            if "api_keys" in data:
                value = data["api_keys"]
                safe["credential_shape"] = type(value).__name__
                if isinstance(value, (list, dict)):
                    safe["credential_count"] = len(value)
                if isinstance(value, list) and value:
                    safe["credential_item_shape"] = type(value[0]).__name__
                    if isinstance(value[0], dict):
                        safe["credential_item_fields"] = [
                            key for key in ("key", "api_key", "value", "enabled", "status")
                            if key in value[0]
                        ]
            if "voice_prompt" in data:
                safe["voice_prompt_length"] = len(data["voice_prompt"] or "")
        print(label, dict(type=row["type"], config_id=row["config_id"], provider=row["provider"],
                          name=row["name"], enabled=bool(row["enabled"]), default=bool(row["is_default"]),
                          keys=sorted(data.keys()), safe=safe))


def main() -> None:
    config = json.loads((PRO_ROOT / "config/config.json").read_text(encoding="utf-8"))
    print("pro_database_type=", config["database"].get("type"))
    sqlite_path = config["database"].get("sqlite", {}).get("file_path")
    if sqlite_path:
        source = Path(sqlite_path)
        if not source.is_absolute():
            source = PRO_ROOT / source
        inspect("pro", source.resolve())
    inspect("pr281", TEST_DB)


if __name__ == "__main__":
    main()
