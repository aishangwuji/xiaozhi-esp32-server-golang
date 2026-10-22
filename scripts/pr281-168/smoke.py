#!/usr/bin/env python3
"""Read-only API and WebSocket smoke checks for the isolated PR281 stack."""

import base64
import hashlib
import hmac
import json
import re
import socket
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


BASE = "http://127.0.0.1:18180"


def request_json(path: str, *, payload=None, headers=None):
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        BASE + path,
        data=data,
        headers={"Content-Type": "application/json", **(headers or {})},
        method="POST" if data is not None else "GET",
    )
    try:
        with urllib.request.urlopen(request, timeout=6) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as error:
        return error.code, json.load(error)


def captcha_fields():
    status, challenge = request_json("/api/captcha/challenge")
    assert status == 200
    match = re.fullmatch(r"(\d+) ([+-]) (\d+) = \?", challenge["prompt"])
    assert match is not None
    left, operator, right = match.groups()
    result = int(left) + int(right) if operator == "+" else int(left) - int(right)
    return {"captchaId": challenge["captchaId"], "captchaAnswer": str(result)}


def old_default_token():
    def encode(value):
        return base64.urlsafe_b64encode(json.dumps(value, separators=(",", ":")).encode()).rstrip(b"=")

    header = encode({"alg": "HS256", "typ": "JWT"})
    claims = encode({"user_id": 1, "username": "admin", "role": "admin", "exp": int(time.time()) + 3600})
    message = header + b"." + claims
    signature = base64.urlsafe_b64encode(hmac.new(b"xiaozhi_admin_secret_key", message, hashlib.sha256).digest()).rstrip(b"=")
    return (message + b"." + signature).decode()


def main():
    root = Path(sys.argv[1]).resolve(strict=True)
    if root.name != "pr281-sqlite" or root.parent != Path("/home/hackers365/shijingbo/git"):
        raise SystemExit("unexpected deployment directory")
    credentials = json.loads((root / "deployment/credentials.json").read_text(encoding="utf-8"))

    status, setup = request_json("/api/setup/status")
    assert status == 200 and setup["needs_setup"] is False

    status, _ = request_json("/api/login", payload={
        "username": "admin", "password": "admin123", **captcha_fields(),
    })
    assert status == 401, f"hardcoded admin login returned {status}"

    status, login = request_json("/api/login", payload={
        "username": credentials["admin_username"],
        "password": credentials["admin_password"],
        **captcha_fields(),
    })
    assert status == 200 and login["user"]["role"] == "admin"
    token = login["token"]

    status, profile = request_json("/api/profile", headers={"Authorization": "Bearer " + token})
    assert status == 200 and profile["user"]["username"] == credentials["admin_username"]

    status, _ = request_json("/api/profile", headers={"Authorization": "Bearer " + old_default_token()})
    assert status == 401, f"old default JWT accepted: {status}"

    voice_sets = []
    for model in ("qwen3-tts-flash", "qwen-audio-3.0-tts-flash"):
        query = urllib.parse.urlencode({"provider": "aliyun_qwen", "model": model})
        status, voices = request_json("/api/user/voice-options?" + query, headers={"Authorization": "Bearer " + token})
        assert status == 200 and voices["data"]
        voice_sets.append(voices["data"])
    assert voice_sets[0][0]["value"] != voice_sets[1][0]["value"]

    status, _ = request_json("/api/system/configs", headers={
        "Authorization": "Bearer " + credentials["internal_auth_token"]
    })
    assert status == 200

    status, device_configs = request_json("/api/configs?device_id=pr281-e2e-fixture", headers={
        "Authorization": "Bearer " + credentials["internal_auth_token"]
    })
    assert status == 200
    selected = device_configs["data"]
    assert selected["config_source"] == "default_global_role"
    expected = {
        "vad": ("silero_vad", None),
        "asr": ("aliyun_funasr", "fun-asr-realtime"),
        "llm": ("openai", "qwen-plus"),
        "tts": ("aliyun_qwen", "qwen-audio-3.0-tts-flash"),
    }
    for kind, (provider, model) in expected.items():
        assert selected[kind]["provider"] == provider
        settings = json.loads(selected[kind]["json_data"])
        if model:
            assert settings.get("model") == model or settings.get("model_name") == model
        if kind != "vad":
            assert isinstance(settings.get("api_key"), str) and settings["api_key"]
            assert "api_keys" not in settings

    with socket.create_connection(("127.0.0.1", 18989), timeout=4) as stream:
        stream.sendall(
            b"GET /xiaozhi/v1/ HTTP/1.1\r\nHost: 127.0.0.1:18989\r\n"
            b"Upgrade: websocket\r\nConnection: Upgrade\r\n"
            b"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"
            b"Sec-WebSocket-Version: 13\r\nDevice-Id: pr281-smoke\r\n\r\n"
        )
        response = stream.recv(1024)
    assert response.startswith(b"HTTP/1.1 101 "), response.split(b"\r\n", 1)[0]
    print("smoke passed: SQLite, login, JWT, model-specific voices, four default configs, WebSocket")


if __name__ == "__main__":
    main()
