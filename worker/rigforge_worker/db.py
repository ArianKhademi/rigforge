"""The worker's view of Postgres: it reads a job and updates the jobs and
assets rows. The api owns the schema (api/internal/store/migrations).

After every write the worker publishes on a Redis pub/sub channel. The message
carries no state; it only tells the api's SSE handler "this job changed, read
the row again". Postgres stays the single source of truth.
"""

from __future__ import annotations

import logging
import time
from datetime import UTC, datetime

import psycopg
import redis

from .worker import JobRow

log = logging.getLogger("rigforge.db")

EVENT_CHANNEL_PREFIX = "job-events:"  # must match api/internal/job/redis.go


class PostgresJobStore:
    def __init__(self, database_url: str, events: redis.Redis | None = None):
        self._url = database_url
        self._events = events
        self._conn: psycopg.Connection | None = None
        # Progress callbacks can fire many times per second; writes are
        # throttled per job so a long video is not thousands of UPDATEs.
        self._last_progress: tuple[str, str, float] = ("", "", 0.0)

    # ---- connection handling ----

    def _connection(self) -> psycopg.Connection:
        if self._conn is None or self._conn.closed:
            # autocommit: each statement is its own transaction unless a block
            # below opens one explicitly with conn.transaction().
            self._conn = psycopg.connect(self._url, autocommit=True)
        return self._conn

    def _run(self, fn):
        """Run fn(conn), reconnecting once if the connection has gone away
        (a Postgres restart must not kill a worker in the middle of a job)."""
        try:
            return fn(self._connection())
        except psycopg.OperationalError:
            log.warning("postgres connection lost; reconnecting")
            if self._conn is not None:
                self._conn.close()
            self._conn = None
            return fn(self._connection())

    def close(self) -> None:
        if self._conn is not None:
            self._conn.close()

    def _publish(self, job_id: str) -> None:
        if self._events is None:
            return
        try:
            self._events.publish(EVENT_CHANNEL_PREFIX + job_id, "changed")
        except redis.RedisError:
            # Only costs liveness: the SSE stream re-reads the row every few
            # seconds anyway.
            log.warning("could not publish job event for %s", job_id)

    # ---- JobStore protocol ----

    def load(self, job_id: str) -> JobRow | None:
        def query(conn):
            return conn.execute(
                """
                SELECT j.id::text, j.asset_id::text, j.status, j.character_id, a.source_key, j.error
                FROM jobs j JOIN assets a ON a.id = j.asset_id
                WHERE j.id = %s
                """,
                (job_id,),
            ).fetchone()

        row = self._run(query)
        if row is None:
            return None
        return JobRow(id=row[0], asset_id=row[1], status=row[2], character_id=row[3], source_key=row[4], error=row[5])

    def begin_attempt(self, job_id: str, attempt: int) -> None:
        # The previous attempt's error is kept (not cleared) while a retry
        # runs, so the UI can show "attempt 2 of 3, last error: ...".
        self._run(
            lambda conn: conn.execute(
                """
                UPDATE jobs SET status = 'transcoding', progress = 0, attempt = %s, retry_at = NULL,
                    started_at = now(), finished_at = NULL, updated_at = now()
                WHERE id = %s
                """,
                (attempt, job_id),
            )
        )
        self._last_progress = (job_id, "transcoding", time.monotonic())
        self._publish(job_id)

    def set_stage(self, job_id: str, status: str, progress: int) -> None:
        # Always write a stage change; within a stage write at most twice a second.
        last_job, last_status, last_time = self._last_progress
        now = time.monotonic()
        if (last_job, last_status) == (job_id, status) and now - last_time < 0.5:
            return
        self._last_progress = (job_id, status, now)
        progress = max(0, min(100, int(progress)))
        self._run(
            lambda conn: conn.execute(
                "UPDATE jobs SET status = %s, progress = %s, updated_at = now() WHERE id = %s",
                (status, progress, job_id),
            )
        )
        self._publish(job_id)

    def schedule_retry(self, job_id: str, next_attempt: int, retry_at: float, error: str) -> None:
        self._run(
            lambda conn: conn.execute(
                """
                UPDATE jobs SET status = 'queued', progress = 0, attempt = %s, retry_at = %s,
                    error = %s, updated_at = now()
                WHERE id = %s
                """,
                (next_attempt, datetime.fromtimestamp(retry_at, tz=UTC), error, job_id),
            )
        )
        self._publish(job_id)

    def fail(self, job_id: str, error: str) -> None:
        def update(conn):
            # Job and asset change together or not at all.
            with conn.transaction():
                conn.execute(
                    """
                    UPDATE jobs SET status = 'failed', error = %s, retry_at = NULL,
                        finished_at = now(), updated_at = now()
                    WHERE id = %s
                    """,
                    (error, job_id),
                )
                conn.execute(
                    """
                    UPDATE assets SET status = 'failed', updated_at = now()
                    WHERE id = (SELECT asset_id FROM jobs WHERE id = %s)
                    """,
                    (job_id,),
                )

        self._run(update)
        self._publish(job_id)

    def complete(self, job_id: str, metadata: dict) -> None:
        def update(conn):
            with conn.transaction():
                conn.execute(
                    """
                    UPDATE jobs SET status = 'done', progress = 100, error = NULL, retry_at = NULL,
                        finished_at = now(), updated_at = now()
                    WHERE id = %s
                    """,
                    (job_id,),
                )
                conn.execute(
                    """
                    UPDATE assets SET status = 'ready', duration_seconds = %s, width = %s, height = %s,
                        fps = %s, frame_count = %s, updated_at = now()
                    WHERE id = (SELECT asset_id FROM jobs WHERE id = %s)
                    """,
                    (
                        metadata.get("duration_seconds"),
                        metadata.get("width"),
                        metadata.get("height"),
                        metadata.get("fps"),
                        metadata.get("frame_count"),
                        job_id,
                    ),
                )

        self._run(update)
        self._publish(job_id)
