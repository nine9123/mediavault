#!/usr/bin/env python3
"""facerec must exit 0 on SIGTERM and SIGINT: idlegate stops it with docker stop.

Starts app.py as a subprocess, waits for /healthz, signals it and checks the
exit code. stdlib only, so it runs in the built image as well as on a laptop:

    python facerec/test_shutdown.py                         # stubs dlib if missing
    docker run --rm -i --entrypoint python IMAGE - < facerec/test_shutdown.py

The subprocess is not PID 1, so this can't catch a broken Dockerfile CMD;
`docker stop facerec` and its exit code cover that.
"""
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
import urllib.request

def _app():
    """app.py next to this file, else the image's copy (when read from stdin)."""
    here = os.path.join(os.path.dirname(os.path.abspath(__file__)), "app.py") if "__file__" in globals() else ""
    return here if os.path.exists(here) else "/app/app.py"


APP = os.environ.get("FACEREC_APP") or _app()

# Stands in for face_recognition when dlib isn't installed: the shutdown path
# never touches it, and building dlib to test a signal handler is 15 minutes.
STUB = "def face_locations(*a, **k): return []\ndef face_encodings(*a, **k): return []\n"


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class ShutdownTest(unittest.TestCase):
    def setUp(self):
        self.env = dict(os.environ, PORT=str(_free_port()), PYTHONUNBUFFERED="1")
        try:
            import face_recognition  # noqa: F401
        except ImportError:
            stubs = tempfile.mkdtemp()
            with open(os.path.join(stubs, "face_recognition.py"), "w") as f:
                f.write(STUB)
            self.env["PYTHONPATH"] = os.pathsep.join(filter(None, [stubs, self.env.get("PYTHONPATH")]))

    def _start(self):
        p = subprocess.Popen([sys.executable, APP], env=self.env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        self.addCleanup(p.stdout.close)
        self.addCleanup(lambda: p.poll() is None and p.kill())
        url = "http://127.0.0.1:%s/healthz" % self.env["PORT"]
        deadline = time.monotonic() + 60  # the real model takes a few seconds to load
        while time.monotonic() < deadline:
            self.assertIsNone(p.poll(), "facerec exited before it was ready")
            try:
                with urllib.request.urlopen(url, timeout=1) as r:
                    if r.status == 200:
                        return p
            except OSError:
                pass
            time.sleep(0.2)
        self.fail("facerec never answered /healthz")

    def _stop(self, sig):
        p = self._start()
        started = time.monotonic()
        p.send_signal(sig)
        code = p.wait(timeout=6)
        took = time.monotonic() - started
        out = p.stdout.read().decode("utf-8", "replace")
        self.assertEqual(code, 0, out)
        self.assertLess(took, 5, out)
        self.assertIn("received %s" % signal.Signals(sig).name, out)
        self.assertIn("shutdown complete", out)

    def test_sigterm_exits_zero(self):
        self._stop(signal.SIGTERM)

    def test_sigint_exits_zero(self):
        self._stop(signal.SIGINT)


if __name__ == "__main__":
    unittest.main()
