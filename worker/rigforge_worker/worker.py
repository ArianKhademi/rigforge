"""The worker loop: take a message, run the handler, and turn the outcome into
the right queue transition and job-row update.

The loop knows nothing about video. It is given a handler (the pipeline) and a
JobStore (Postgres in production, an in-memory fake in tests), which is what
lets the queue semantics be tested with a real Redis but without ffmpeg.
"""

from __future__ import annotations

import logging
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Protocol

from .errors import PermanentError
from .queue import JobQueue, Message

log = logging.getLogger("rigforge.worker")

# Job statuses, shared with the api (api/internal/store/store.go).
QUEUED, DONE, FAILED = "queued", "done", "failed"


@dataclass(frozen=True)
class JobRow:
    id: str
    asset_id: str
    status: str
    character_id: str
    source_key: str
    error: str | None = None


class JobStore(Protocol):
    """The job and asset rows the worker reads and updates."""

    def load(self, job_id: str) -> JobRow | None: ...
    def begin_attempt(self, job_id: str, attempt: int) -> None: ...
    def set_stage(self, job_id: str, status: str, progress: int) -> bool:
        """Record progress. Returns False if the job row no longer exists."""
        ...

    def schedule_retry(self, job_id: str, next_attempt: int, retry_at: float, error: str) -> None: ...
    def fail(self, job_id: str, error: str) -> None: ...
    def complete(self, job_id: str, metadata: dict) -> None: ...


class JobGone(Exception):
    """The job row disappeared while the job was running: the user deleted
    the asset. The worker stops and drops the message; there is nobody left to
    produce outputs for."""


# A handler processes one job. It reports progress through the callback and
# returns asset metadata (duration, resolution, ...) to store on success.
ProgressFn = Callable[[str, int], None]
Handler = Callable[[JobRow, ProgressFn], dict]


class Heartbeat:
    """Context manager that heartbeats a message from a background thread
    while the (blocking, CPU-heavy) handler runs on the main thread."""

    def __init__(self, queue: JobQueue, msg: Message, interval: float):
        self._queue, self._msg, self._interval = queue, msg, interval
        self._stop = threading.Event()
        self._thread = threading.Thread(target=self._run, name="heartbeat", daemon=True)

    def _run(self) -> None:
        # Event.wait returns False on timeout (keep beating) and True once
        # stop is set (exit), so the thread ends promptly when the job does.
        while not self._stop.wait(self._interval):
            try:
                self._queue.heartbeat(self._msg)
            except Exception:  # a missed beat is survivable; the next one retries
                log.exception("heartbeat failed for job %s", self._msg.job_id)

    def __enter__(self) -> Heartbeat:
        self._thread.start()
        return self

    def __exit__(self, *exc) -> None:
        self._stop.set()
        self._thread.join(timeout=5)


class Worker:
    def __init__(
        self,
        queue: JobQueue,
        store: JobStore,
        handler: Handler,
        heartbeat_interval: float = 15.0,
    ):
        self.queue = queue
        self.store = store
        self.handler = handler
        self.heartbeat_interval = heartbeat_interval
        self._stopping = threading.Event()

    def stop(self) -> None:
        """Finish the current job, then exit the loop (SIGTERM handler)."""
        self._stopping.set()

    def run_forever(self) -> None:
        self.queue.ensure_group()
        log.info("worker %s consuming", self.queue.consumer)
        while not self._stopping.is_set():
            try:
                self.run_once()
            except Exception:
                # Redis or Postgres hiccup outside a job: log, back off, retry.
                log.exception("worker loop error")
                time.sleep(1.0)

    def run_once(self) -> bool:
        """Process at most one message. Returns True if one was handled."""
        msg = self.queue.next()
        if msg is None:
            return False
        self.process(msg)
        return True

    def process(self, msg: Message) -> None:
        job = self.store.load(msg.job_id)

        # Idempotency guards. A message can be delivered more than once (see
        # queue.py), so first check what the job row already says.
        if job is None:
            # The asset was deleted while the job was queued.
            log.info("job %s no longer exists; dropping message", msg.job_id)
            self.queue.ack(msg)
            return
        if job.status == DONE:
            # A previous delivery finished the work but died before acking.
            log.info("job %s already done; acking duplicate delivery", msg.job_id)
            self.queue.ack(msg)
            return
        if job.status == FAILED:
            # A previous delivery marked the row failed but died before the
            # message reached the dead-letter stream. Finish that move.
            self.queue.dead_letter(msg, job.error or "failed")
            return

        log.info("job %s attempt %d starting%s", msg.job_id, msg.attempt, " (reclaimed)" if msg.reclaimed else "")
        self.store.begin_attempt(msg.job_id, msg.attempt)

        def progress(status: str, percent: int) -> None:
            # Every stage change doubles as a check that the job is still
            # wanted, so a deleted asset stops costing CPU at the next stage
            # boundary and never gets output files written for it.
            if self.store.set_stage(msg.job_id, status, percent) is False:
                raise JobGone(msg.job_id)

        try:
            with Heartbeat(self.queue, msg, self.heartbeat_interval):
                metadata = self.handler(job, progress)
        except JobGone:
            log.info("job %s was deleted while running; stopping", msg.job_id)
            self.queue.ack(msg)
        except PermanentError as exc:
            # Retrying cannot help. Row first, then queue: if we die in
            # between, the FAILED guard above completes the move on redelivery.
            log.warning("job %s failed permanently: %s", msg.job_id, exc)
            self.store.fail(msg.job_id, str(exc))
            self.queue.dead_letter(msg, str(exc))
        except Exception as exc:
            error = f"{type(exc).__name__}: {exc}"
            log.exception("job %s attempt %d failed", msg.job_id, msg.attempt)
            if msg.attempt >= self.queue.cfg.max_attempts:
                self.store.fail(msg.job_id, error)
                self.queue.dead_letter(msg, error)
            else:
                decision = self.queue.retry(msg, error)
                self.store.schedule_retry(msg.job_id, decision.next_attempt, decision.retry_at, error)
        else:
            # Row first, then ack: if we die in between, the DONE guard above
            # acks the redelivered message without redoing the work.
            self.store.complete(msg.job_id, metadata)
            self.queue.ack(msg)
            log.info("job %s done", msg.job_id)
