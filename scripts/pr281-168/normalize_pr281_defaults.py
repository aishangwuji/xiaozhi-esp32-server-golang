#!/usr/bin/env python3
"""One-time, backed-up key-pool conversion for the already imported PR281 DB."""

import os
import sqlite3
from pathlib import Path

from import_pro_defaults import ROOT, TARGET, TYPES, runtime_config


BACKUP = ROOT / "deployment/pr281-before-key-normalization.db"


def main() -> None:
    assert ROOT.is_dir() and TARGET.is_file()
    if BACKUP.exists():
        raise SystemExit("backup already exists; refusing to normalize twice")
    db = sqlite3.connect(f"file:{TARGET}?mode=rw", uri=True)
    try:
        records = db.execute(
            "SELECT id, type, json_data FROM configs WHERE type IN (?, ?, ?, ?) AND enabled = 1 AND is_default = 1",
            TYPES,
        ).fetchall()
        if len(records) != 4 or {row[1] for row in records} != set(TYPES):
            raise SystemExit("expected exactly four imported defaults")
        updates = [(runtime_config(kind, raw), row_id) for row_id, kind, raw in records]
        fd = os.open(BACKUP, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        os.close(fd)
        backup = sqlite3.connect(BACKUP)
        try:
            db.backup(backup)
        finally:
            backup.close()
        with db:
            db.executemany("UPDATE configs SET json_data = ? WHERE id = ?", updates)
        print("normalized enabled credential for asr, llm, tts; backup:", BACKUP)
    finally:
        db.close()


if __name__ == "__main__":
    main()
