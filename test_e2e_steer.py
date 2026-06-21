
"""
E2E test: Spawns cove.exe in a Windows PTY, drives it with real keystrokes,
and verifies the steer flow works end-to-end.

Requires: pywinpty (pip install pywinpty)
Run:      python test_e2e_steer.py
"""

import json
import os
import sys
import tempfile
import threading
import time
import winpty
from http.server import HTTPServer, BaseHTTPRequestHandler


# ── Mock LLM Server ────────────────────────────────────────────────────────

class MockLLMHandler(BaseHTTPRequestHandler):
    call_count = 0
    requests_log = []
    _lock = threading.Lock()

    def do_POST(self):
        print(f"  [SERVER] POST {self.path} from {self.client_address}")
        if not self.path.endswith("/chat/completions"):
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        with self._lock:
            self.call_count += 1
            call_num = self.call_count
            self.requests_log.append(body)
        if call_num == 1:
            resp = {
                "choices": [{"message": {"role": "assistant", "content": "",
                    "tool_calls": [{"id": "tc1", "type": "function",
                        "function": {"name": "shell",
                            "arguments": json.dumps({"command": "echo file1.go file2.py file3.go"})}}]}}],
                "usage": {"prompt_tokens": 10, "completion_tokens": 5}}
        else:
            resp = {
                "choices": [{"message": {"role": "assistant",
                    "content": "FILTERED: file1.go, file3.go (steer applied)"}}],
                "usage": {"prompt_tokens": 20, "completion_tokens": 10}}
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(resp).encode())

    def log_message(self, format, *args):
        pass


def main():
    print("=" * 60)
    print("  cove Steer E2E Test -- Real PTY + Real HTTP")
    print("=" * 60)
    print()

    # 1. Start mock LLM server
    server = HTTPServer(("127.0.0.1", 0), MockLLMHandler)
    port = server.server_address[1]
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    print(f"[OK] Mock server: http://127.0.0.1:{port}")

    # 2. Find cove.exe
    cove_exe = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cove.exe")
    if not os.path.exists(cove_exe):
        print(f"[FAIL] cove.exe not found at {cove_exe}")
        sys.exit(1)
    print(f"[OK] cove.exe: {cove_exe}")

    # 3. Create temp config
    config_dir = tempfile.mkdtemp(prefix="cove_e2e_")
    config = {
        "model": "test-model",
        "provider": {
            "name": "openai",
            "api_key": "sk-test",
            "base_url": f"http://127.0.0.1:{port}/v1",
        },
        "permission_mode": "bypass",
        "max_budget_usd": 100,
    }
    config_path = os.path.join(config_dir, "config.json")
    with open(config_path, "w") as f:
        json.dump(config, f)
    print(f"[OK] Config: {config_path}")

    # 4. Spawn cove.exe in PTY
    env = os.environ.copy()
    env["COVE_CONFIG_DIR"] = config_dir
    env["COVE_TUI"] = "1"

    print("[OK] Spawning cove.exe...")
    pty = winpty.PtyProcess.spawn(cove_exe, dimensions=(30, 100), env=env)
    print(f"     PID: {pty.pid}")

    # 5. Wait for TUI and read initial output
    time.sleep(3.0)
    startup = ""
    for _ in range(5):
        try:
            raw = pty.read(1024)
            if raw:
                startup += raw.decode("utf-8", errors="replace") if isinstance(raw, bytes) else str(raw)
        except Exception:
            pass
        time.sleep(0.2)
    print(f"[OK] Startup output: {len(startup)} bytes")
    # Print first 500 chars of startup
    printable = startup.replace("\x1b", "[ESC]")[:500]
    print(f"     First 500: {printable}")

    # 6. Check if process is still alive
    if not pty.isalive():
        print("[FAIL] cove.exe died on startup!")
        pty.close()
        server.shutdown()
        sys.exit(1)

    # 7. Send command character by character
    print()
    print(">>> Typing: list files")
    for ch in "list files":
        pty.write(ch)
        time.sleep(0.02)
    pty.write("\r")  # Enter
    time.sleep(2.0)

    # Read PTY to see what happened
    try:
        mid = pty.read(16384)
        mid_out = mid.decode("utf-8", errors="replace") if isinstance(mid, bytes) else str(mid)
    except Exception as e:
        mid_out = f"<read error: {e}>"
    print(f"[OK] After first input: {len(mid_out)} bytes")

    # 8. Send steer
    print(">>> Typing steer: only Go files")
    for ch in "only Go files":
        pty.write(ch)
        time.sleep(0.02)
    pty.write("\r")  # Enter
    time.sleep(5.0)

    # 9. Read final PTY output
    try:
        raw = pty.read(16384)
        output = raw.decode("utf-8", errors="replace") if isinstance(raw, bytes) else str(raw)
    except Exception:
        output = ""

    # 10. Verify
    print()
    print("=" * 60)
    print("  RESULTS")
    print("=" * 60)

    with MockLLMHandler._lock:
        calls = MockLLMHandler.call_count
        reqs = list(MockLLMHandler.requests_log)

    print(f"API calls: {calls}")
    if calls >= 2:
        print("[PASS] Server received >= 2 requests")
        req2 = reqs[1]
        msgs = req2.get("messages", [])
        found = False
        for msg in msgs:
            c = msg.get("content", "")
            if isinstance(c, str) and "only Go files" in c:
                found = True
                print("[PASS] Steer text 'only Go files' found in 2nd request!")
                print(f"       Tool msg: ...{c[-100:]}")
                break
        if not found:
            print("[FAIL] Steer text NOT in 2nd request")
            for i, msg in enumerate(msgs):
                c = msg.get("content", "")
                if isinstance(c, str) and c:
                    print(f"       [{i}] {msg.get('role')}: {c[:100]}")
    else:
        print(f"[FAIL] Only {calls} requests")

    # 11. Cleanup
    pty.close()
    server.shutdown()
    # Clean config
    try:
        os.remove(config_path)
        os.rmdir(config_dir)
    except Exception:
        pass
    print()
    print("[DONE]")


if __name__ == "__main__":
    main()
