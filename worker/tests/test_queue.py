"""Queue semantics against a real Redis: success, retry with backoff,
dead-lettering after three attempts, and crash-and-reclaim.

The handler is a stub and the job table is a fake (tests/fakes.py), so these
tests are only about delivery guarantees, not video processing.
"""

from __future__ import annotations

import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from rigforge_worker.errors import PermanentError
from rigforge_worker.queue import JobQueue, QueueConfig
from rigforge_worker.worker import Worker

from fakes import RedisJobStore

pytestmark = pytest.mark.redis


def make_config(prefix: str, **overrides) -> QueueConfig:
    defaults = dict(
        stream=f"{prefix}:jobs",
        delayed_key=f"{prefix}:delayed",
        dead_stream=f"{prefix}:dead",
        block_ms=50,
    )
    return QueueConfig(**{**defaults, **overrides})


class Harness:
    """One worker with a scripted handler."""

    def __init__(self, rdb, prefix, consumer="worker-a", **config):
        self.rdb = rdb
        self.cfg = make_config(prefix, **config)
        self.queue = JobQueue(rdb, consumer, self.cfg)
        self.queue.ensure_group()
        self.store = RedisJobStore(rdb, prefix)
        self.calls: list[int] = []  # attempt number of every handler call
        self.outcomes: list[Exception | None] = []  # scripted result per call
        self.worker = Worker(self.queue, self.store, self._handler, heartbeat_interval=0.2)

    def _handler(self, job, progress):
        self.calls.append(len(self.calls) + 1)
        progress("extracting", 50)
        outcome = self.outcomes.pop(0) if self.outcomes else None
        if outcome is not None:
            raise outcome
        return {"frames": 150}

    def submit(self, job_id="job-1") -> str:
        self.store.create(job_id)
        self.queue.enqueue(job_id, "asset-1")
        return job_id

    # ---- what Redis holds ----

    def stream_len(self) -> int:
        return self.rdb.xlen(self.cfg.stream)

    def pending(self) -> int:
        return self.rdb.xpending(self.cfg.stream, self.cfg.group)["pending"]

    def delayed(self) -> list[tuple[str, float]]:
        return self.rdb.zrange(self.cfg.delayed_key, 0, -1, withscores=True)

    def dead(self) -> list[dict]:
        return [fields for _id, fields in self.rdb.xrange(self.cfg.dead_stream)]

    def assert_queue_empty(self):
        assert self.stream_len() == 0, "message left on the stream"
        assert self.pending() == 0, "message left pending"
        assert self.delayed() == [], "retry left scheduled"


def test_success_acks_and_marks_done(rdb, prefix):
    h = Harness(rdb, prefix)
    job = h.submit()

    assert h.worker.run_once() is True

    row = h.store.row(job)
    assert row["status"] == "done"
    assert row["history"] == ["begin:1", "stage:extracting:50", "complete"]
    h.assert_queue_empty()
    assert h.dead() == []
    # Nothing left to do.
    assert h.worker.run_once() is False


def test_jobs_enqueued_before_any_worker_started_are_delivered(rdb, prefix):
    cfg = make_config(prefix)
    # The api enqueues while no worker (and no consumer group) exists yet.
    rdb.xadd(cfg.stream, {"jobId": "early", "assetId": "a", "type": "process_video", "attempt": "1"})

    h = Harness(rdb, prefix)
    h.store.create("early")
    assert h.worker.run_once() is True
    assert h.store.row("early")["status"] == "done"


def test_failed_job_is_retried_with_backoff_then_dead_lettered(rdb, prefix):
    h = Harness(rdb, prefix)  # default config: 3 attempts, backoff 60s / 300s / 900s
    h.outcomes = [RuntimeError("storage timeout")] * 3
    job = h.submit()

    # Attempt 1 fails: the job waits in the delayed set for 1 minute.
    before = time.time()
    h.worker.run_once()
    assert h.calls == [1]
    row = h.store.row(job)
    assert (row["status"], row["attempt"]) == ("queued", "2")
    assert "storage timeout" in row["error"]
    [(_, due)] = h.delayed()
    assert 60 <= due - before <= 62, "first retry must be scheduled 1 minute out"
    assert h.stream_len() == 0 and h.pending() == 0

    # Not due yet: nothing is promoted and the worker has nothing to run.
    assert h.queue.promote_due(now=before + 59) == 0
    assert h.worker.run_once() is False

    # After the backoff the retry is promoted back onto the stream as attempt 2.
    assert h.queue.promote_due(now=due + 0.001) == 1
    assert h.delayed() == []
    before = time.time()
    h.worker.run_once()
    assert h.calls == [1, 2]
    [(_, due)] = h.delayed()
    assert 300 <= due - before <= 302, "second retry must be scheduled 5 minutes out"
    assert h.store.row(job)["attempt"] == "3"

    # Attempt 3 fails: the budget is spent, so the job goes to the dead-letter
    # stream with its last error instead of being scheduled again.
    assert h.queue.promote_due(now=due + 0.001) == 1
    h.worker.run_once()
    assert h.calls == [1, 2, 3]
    h.assert_queue_empty()
    [dead] = h.dead()
    assert dead["jobId"] == job and dead["attempt"] == "3"
    assert "storage timeout" in dead["error"]
    row = h.store.row(job)
    assert row["status"] == "failed" and "storage timeout" in row["error"]

    # A dead job is never run again.
    assert h.queue.promote_due(now=time.time() + 10_000) == 0
    assert h.worker.run_once() is False
    assert h.calls == [1, 2, 3]


def test_job_that_recovers_on_retry_completes(rdb, prefix):
    h = Harness(rdb, prefix)
    h.outcomes = [ConnectionError("bucket unreachable"), None]
    job = h.submit()

    h.worker.run_once()
    assert h.store.row(job)["status"] == "queued"
    h.queue.promote_due(now=time.time() + 61)
    h.worker.run_once()

    row = h.store.row(job)
    assert row["status"] == "done" and row["error"] == ""
    assert h.calls == [1, 2]
    h.assert_queue_empty()
    assert h.dead() == []


def test_permanent_error_is_dead_lettered_without_retries(rdb, prefix):
    h = Harness(rdb, prefix)
    h.outcomes = [PermanentError("no person detected in the video")]
    job = h.submit()

    h.worker.run_once()

    assert h.calls == [1], "a permanent failure must not be retried"
    h.assert_queue_empty()
    [dead] = h.dead()
    assert dead["attempt"] == "1" and dead["error"] == "no person detected in the video"
    assert h.store.row(job)["status"] == "failed"


def test_promoting_a_retry_is_atomic(rdb, prefix):
    h = Harness(rdb, prefix)
    other = JobQueue(rdb, "worker-b", h.cfg)
    h.outcomes = [RuntimeError("boom")]
    h.submit()
    h.worker.run_once()

    # Two workers notice the due retry at the same moment: exactly one of them
    # moves it, so the job is on the stream once, not twice.
    future = time.time() + 61
    assert sorted([h.queue.promote_due(now=future), other.promote_due(now=future)]) == [0, 1]
    assert h.stream_len() == 1


def test_duplicate_delivery_of_a_finished_job_is_acked_not_rerun(rdb, prefix):
    h = Harness(rdb, prefix)
    job = h.submit()
    h.worker.run_once()
    assert h.calls == [1]

    # The same job shows up again (the api's dispatcher enqueued it twice, or a
    # worker died between marking the row done and acking).
    h.queue.enqueue(job, "asset-1")
    h.worker.run_once()

    assert h.calls == [1], "idempotency: a done job must not be processed again"
    h.assert_queue_empty()


def test_message_for_a_deleted_job_is_dropped(rdb, prefix):
    h = Harness(rdb, prefix)
    job = h.submit()
    h.store.delete(job)  # the user deleted the asset while the job was queued

    h.worker.run_once()

    assert h.calls == []
    h.assert_queue_empty()
    assert h.dead() == []


def test_row_marked_failed_but_not_yet_dead_lettered_is_finished_on_redelivery(rdb, prefix):
    h = Harness(rdb, prefix)
    job = h.submit()
    # A worker marked the row failed and died before moving the message.
    h.store.fail(job, "ffprobe: not a video")

    h.worker.run_once()

    assert h.calls == []
    [dead] = h.dead()
    assert dead["error"] == "ffprobe: not a video"
    h.assert_queue_empty()


def test_message_that_keeps_killing_workers_is_dead_lettered(rdb, prefix):
    # Visibility timeout 0: any pending message is immediately reclaimable.
    cfg = dict(visibility_timeout_ms=0, max_deliveries=2)
    first = Harness(rdb, prefix, consumer="w1", **cfg)
    first.submit()

    # w1 takes the job and "crashes" (never acks). Delivery 1.
    assert first.queue.next() is not None
    # w2 reclaims it and also crashes. Delivery 2.
    second = JobQueue(rdb, "w2", first.cfg)
    msg = second.next()
    assert msg is not None and msg.reclaimed
    # A third delivery would exceed max_deliveries: dead-letter instead.
    third = JobQueue(rdb, "w3", first.cfg)
    assert third.next() is None
    [dead] = first.dead()
    assert "crash loop" in dead["error"]
    assert first.pending() == 0 and first.stream_len() == 0


# ---- crash and reclaim, with real processes ----


def spawn(redis_url, prefix, name, mode, visibility_ms):
    env = {**os.environ, "PYTHONPATH": str(Path(__file__).parents[1])}
    script = str(Path(__file__).parent / "consumer_proc.py")
    return subprocess.Popen([sys.executable, script, redis_url, prefix, name, mode, str(visibility_ms)], env=env)


def wait_for(predicate, timeout=15.0, what="condition"):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if predicate():
            return
        time.sleep(0.05)
    pytest.fail(f"timed out waiting for {what}")


def test_job_of_a_killed_worker_is_reclaimed_and_finished_by_another(rdb, redis_url, prefix):
    h = Harness(rdb, prefix)  # used only to enqueue and inspect
    job = h.submit()
    visibility_ms = 1000

    crashing = spawn(redis_url, prefix, "worker-crash", "hang", visibility_ms)
    survivor = None
    try:
        # worker-crash picks the job up and is mid-job...
        wait_for(
            lambda: rdb.lrange(f"{prefix}:started:{job}", 0, -1) == ["worker-crash"],
            what="first worker to start the job",
        )
        # ...when it is killed without any chance to clean up.
        crashing.send_signal(signal.SIGKILL)
        crashing.wait()

        # The job is not lost: it is still pending, owned by the dead consumer.
        [entry] = rdb.xpending_range(h.cfg.stream, h.cfg.group, "-", "+", 10)
        assert entry["consumer"] == "worker-crash"
        assert rdb.lrange(f"{prefix}:finished:{job}", 0, -1) == []

        # A second worker starts. Once the message has been idle for the
        # visibility timeout it claims the job and finishes it.
        survivor = spawn(redis_url, prefix, "worker-survivor", "ok", visibility_ms)
        wait_for(
            lambda: rdb.lrange(f"{prefix}:finished:{job}", 0, -1) == ["worker-survivor"],
            what="second worker to finish the job",
        )
        wait_for(lambda: h.store.row(job)["status"] == "done", what="job row to be marked done")

        assert rdb.lrange(f"{prefix}:started:{job}", 0, -1) == ["worker-crash", "worker-survivor"]
        wait_for(lambda: h.pending() == 0 and h.stream_len() == 0, what="message to be acked")
        assert h.dead() == []
    finally:
        for proc in (crashing, survivor):
            if proc is not None and proc.poll() is None:
                proc.kill()
                proc.wait()


def test_heartbeats_keep_a_slow_job_from_being_stolen(rdb, redis_url, prefix):
    h = Harness(rdb, prefix)
    job = h.submit()
    # The job takes 3 s; the visibility timeout is only 1 s. Without heartbeats
    # the second worker would take it over mid-flight.
    visibility_ms = 1000

    slow = spawn(redis_url, prefix, "worker-slow", "slow", visibility_ms)
    rival = None
    try:
        wait_for(lambda: rdb.llen(f"{prefix}:started:{job}") == 1, what="slow worker to start the job")
        rival = spawn(redis_url, prefix, "worker-rival", "ok", visibility_ms)
        wait_for(lambda: rdb.llen(f"{prefix}:finished:{job}") == 1, what="job to finish")

        assert rdb.lrange(f"{prefix}:started:{job}", 0, -1) == ["worker-slow"], "the job must run exactly once"
        assert rdb.lrange(f"{prefix}:finished:{job}", 0, -1) == ["worker-slow"]
    finally:
        for proc in (slow, rival):
            if proc is not None and proc.poll() is None:
                proc.kill()
                proc.wait()
