# PR #281 isolated test deployment on 192.168.209.168

- Checkout: `/home/hackers365/shijingbo/git/pr281-sqlite`
- SQLite: `manager/backend/data/pr281.db` (mode 0600)
- Manager: `xiaozhi-pr281-manager.service`, loopback port 18180
- WebSocket: `xiaozhi-pr281-server.service`, port 18989
- Frontend: port 18181
- Existing pro services, DB, and web site remain separate.

`import_pro_defaults.py` reads only the four enabled VAD/ASR/LLM/TTS defaults
from the pro SQLite DB, converts active `api_keys` entries to the runtime
`api_key` field, backs up the target DB, and refuses to overwrite existing
voice configs. Both PR281 services must be stopped during the import. The
one-time `normalize_pr281_defaults.py` handled the initial import done before
the key-shape difference was discovered. Neither script prints credentials.

For a smoke check, run `python3 deployment/smoke.py
/home/hackers365/shijingbo/git/pr281-sqlite` on the host. The audio-in/audio-out
check is `go run ./test/pr281_e2e -wav <16kHz-mono-WAV>` from the isolated
checkout, with the server's `LD_LIBRARY_PATH` set. The test asserts nonempty
ASR output, LLM-derived reply sentences, TTS completion, and decodable,
non-silent Opus frames without printing conversation text.
