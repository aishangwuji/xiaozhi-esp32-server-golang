#!/usr/bin/env python3
"""Copy only pro's four active voice defaults into the isolated PR281 SQLite DB.

Run on 168 while both PR281 services are stopped. Never prints config JSON.
"""

import json
import os
import sqlite3
from pathlib import Path


ROOT = Path("/home/hackers365/shijingbo/git/pr281-sqlite")
SOURCE = Path("/home/hackers365/shijingbo/git/xiaozhi-esp32-server-golang-pro/manager/backend/data/xiaozhi.db")
TARGET = ROOT / "manager/backend/data/pr281.db"
BACKUP = ROOT / "deployment/pr281-before-pro-defaults.db"
TYPES = ("vad", "asr", "llm", "tts")
COLUMNS = ("type", "name", "config_id", "provider", "json_data", "enabled", "is_default", "created_at", "updated_at")


def runtime_config(kind: str, raw: str) -> str:
    """Adapt pro's key pool to the single-key format supported by this server."""
    data = json.loads(raw)
    if not isinstance(data, dict):
        raise ValueError("invalid config JSON")
    if kind in ("asr", "llm", "tts"):
        keys = data.pop("api_keys", None)
        if not isinstance(keys, list):
            raise ValueError("expected pro api_keys list")
        active = [entry.get("api_key") for entry in keys if isinstance(entry, dict) and entry.get("enabled") is True
                  and isinstance(entry.get("api_key"), str) and entry["api_key"].strip()]
        if not active:
            raise ValueError("no active key for " + kind)
        data["api_key"] = active[0]
    return json.dumps(data, ensure_ascii=False, separators=(",", ":"))


def main() -> None:
    assert ROOT.is_dir() and ROOT.parent == Path("/home/hackers365/shijingbo/git")
    assert SOURCE.is_file() and TARGET.is_file()
    if BACKUP.exists():
        raise SystemExit("backup already exists; refusing to import twice")

    source = sqlite3.connect(f"file:{SOURCE}?mode=ro", uri=True)
    source.row_factory = sqlite3.Row
    target = sqlite3.connect(f"file:{TARGET}?mode=rw", uri=True, timeout=10)
    try:
        records = source.execute(
            "SELECT " + ", ".join(COLUMNS) + " FROM configs "
            "WHERE type IN (?, ?, ?, ?) AND enabled = 1 AND is_default = 1",
            TYPES,
        ).fetchall()
        if len(records) != len(TYPES) or {row["type"] for row in records} != set(TYPES):
            raise SystemExit("source must have exactly one enabled default for each voice type")
        for row in records:
            data = json.loads(row["json_data"])
            if not isinstance(data, dict):
                raise SystemExit("invalid source config JSON")
            if row["type"] == "vad" and not (ROOT / data.get("model_path", "")).is_file():
                raise SystemExit("VAD model is absent from isolated deployment")
            runtime_config(row["type"], row["json_data"])

        count = target.execute(
            "SELECT COUNT(*) FROM configs WHERE type IN (?, ?, ?, ?)", TYPES
        ).fetchone()[0]
        if count:
            raise SystemExit("target already has voice configs; refusing to overwrite")

        fd = os.open(BACKUP, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.close(fd)
        backup = sqlite3.connect(BACKUP)
        try:
            target.backup(backup)
        finally:
            backup.close()

        placeholders = ", ".join("?" for _ in COLUMNS)
        with target:
            target.executemany(
                "INSERT INTO configs (" + ", ".join(COLUMNS) + ") VALUES (" + placeholders + ")",
                [tuple(runtime_config(row["type"], row["json_data"]) if column == "json_data" else row[column]
                       for column in COLUMNS) for row in records],
            )
        for row in records:
            data = json.loads(row["json_data"])
            print("imported", row["type"], row["provider"], data.get("model", ""))
        print("backup:", BACKUP)
    finally:
        source.close()
        target.close()


if __name__ == "__main__":
    main()
