"""Worker start-up when its dependencies are not ready yet."""

from __future__ import annotations

import redis

from rigforge_worker.worker import Worker


class FlakyQueue:
    """A queue whose Redis is unreachable for the first few attempts."""

    consumer = "test-worker"

    def __init__(self, failures: int):
        self.failures = failures
        self.attempts = 0

    def ensure_group(self) -> None:
        self.attempts += 1
        if self.attempts <= self.failures:
            raise redis.ConnectionError("Error 111 connecting to redis:6379. Connection refused.")


def test_worker_waits_for_redis_instead_of_crashing(tmp_path):
    queue = FlakyQueue(failures=3)
    alive = tmp_path / "worker.alive"
    worker = Worker(queue, store=None, handler=None, alive_file=alive)

    worker.wait_for_queue(retry_seconds=0)

    assert queue.attempts == 4, "should retry until Redis answers"
    # While waiting it still reports itself alive, so the liveness probe does
    # not restart a worker that is merely waiting for a dependency.
    assert alive.exists()


def test_stop_ends_the_wait():
    queue = FlakyQueue(failures=10**9)
    worker = Worker(queue, store=None, handler=None)
    worker.stop()
    worker.wait_for_queue(retry_seconds=0)  # returns instead of looping forever
    assert queue.attempts == 0
