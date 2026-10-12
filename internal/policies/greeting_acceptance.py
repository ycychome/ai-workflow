"""Independent HTTP acceptance owned by the control binary, not the candidate."""

import json
import os
import socket
import subprocess
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen


# Run the project's regression suite, but do not use it as the only acceptance.
environment = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
subprocess.run(["python3", "-B", "-m", "unittest", "-v"], env=environment, check=True, timeout=15)

with socket.socket() as temporary:
    temporary.bind(("127.0.0.1", 0))
    port = temporary.getsockname()[1]

service = subprocess.Popen(
    ["python3", "-I", "-B", "app.py", "--host", "127.0.0.1", "--port", str(port)],
    env=environment,
    stdout=subprocess.DEVNULL,
    stderr=subprocess.DEVNULL,
)
base = f"http://127.0.0.1:{port}"


def request(path, method="GET"):
    try:
        response = urlopen(Request(base + path, method=method), timeout=2)
    except HTTPError as error:
        response = error
    with response:
        assert response.headers.get_content_type() == "application/json", "response must be JSON"
        return response.status, json.loads(response.read().decode("utf-8"))


try:
    deadline = time.monotonic() + 5
    while True:
        assert service.poll() is None, "service exited before readiness"
        try:
            assert request("/healthz") == (200, {"status": "ok"})
            break
        except (OSError, URLError):
            if time.monotonic() >= deadline:
                raise RuntimeError("service did not become ready")
            time.sleep(0.05)

    assert request("/api/greeting") == (200, {"message": "hello M0"}), "default HTTP greeting failed"
    assert request("/api/greeting?name=Codex") == (200, {"message": "hello M0, Codex!"}), "named HTTP greeting failed"
    assert request("/api/greeting?" + urlencode({"name": "  小明  "})) == (200, {"message": "hello M0, 小明!"})
    for query in ("name=", "name=%20", "name=" + "a" * 81, "name=a&name=b", "unknown=x"):
        assert request("/api/greeting?" + query)[0] == 400, "invalid input was accepted"
    assert request("/missing")[0] == 404
    assert request("/api/greeting", method="POST")[0] == 405
    assert request("/healthz") == (200, {"status": "ok"})
    print("M0 trusted HTTP acceptance completed", flush=True)
finally:
    service.terminate()
    try:
        service.wait(timeout=2)
    except subprocess.TimeoutExpired:
        service.kill()
        service.wait(timeout=2)
