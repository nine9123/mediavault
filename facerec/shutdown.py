"""Clean shutdown for containers stopped by idlegate (docker stop -> SIGTERM).

Python is PID 1 in this container, and the kernel drops a signal sent to PID 1
unless that process has a handler for it. Without one, `docker stop` waits out
its grace period and SIGKILLs us, the container exits 137, and TrueNAS marks the
whole app as Crashed. With one, we exit 0 and the app stays Running while
idlegate puts it to sleep.
"""
import atexit
import logging
import os
import signal
import sys
import threading

log = logging.getLogger("shutdown")
DEADLINE_S = 5  # idlegate's stop_grace is 10 s; finish well inside it


def _force_exit():
    log.warning("shutdown still running after %ss, forcing exit 0", DEADLINE_S)
    logging.shutdown()
    os._exit(0)


def _on_signal(signum, _frame):
    log.info("received %s, shutting down", signal.Signals(signum).name)
    t = threading.Timer(DEADLINE_S, _force_exit)
    t.daemon = True  # never keeps the process alive by itself
    t.start()
    sys.exit(0)  # SystemExit in the main thread: servers unwind, atexit runs


def install():
    """Call from the main thread, first thing in app.py."""
    signal.signal(signal.SIGTERM, _on_signal)
    signal.signal(signal.SIGINT, _on_signal)
    atexit.register(lambda: log.info("shutdown complete"))
