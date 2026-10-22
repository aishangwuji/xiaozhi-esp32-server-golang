#!/usr/bin/env python3
"""Initialize the private PR281 SQLite manager before exposing nginx."""

import json
import sys
import time
import urllib.request
from pathlib import Path


def main() -> None:
    root = Path(sys.argv[1]).resolve(strict=True)
    if root.name != "pr281-sqlite" or root.parent != Path("/home/hackers365/shijingbo/git"):
        raise SystemExit("unexpected deployment directory")

    credentials = json.loads((root / "deployment/credentials.json").read_text(encoding="utf-8"))
    base_url = "http://127.0.0.1:18180/api/setup"
    for attempt in range(30):
        try:
            with urllib.request.urlopen(base_url + "/status", timeout=2) as response:
                status = json.load(response)
            break
        except Exception:
            if attempt == 29:
                raise
            time.sleep(1)
    if not status.get("needs_setup"):
        print("manager already initialized")
        return

    payload = {key: credentials[key] for key in ("admin_username", "admin_email", "admin_password")}
    request = urllib.request.Request(
        base_url + "/initialize",
        data=json.dumps(payload).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=20) as response:
        json.load(response)
    with urllib.request.urlopen(base_url + "/status", timeout=3) as response:
        status = json.load(response)
    if status.get("needs_setup"):
        raise SystemExit("manager initialization did not persist")
    print("SQLite manager initialized with admin user", credentials["admin_username"])


if __name__ == "__main__":
    main()
