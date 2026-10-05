"""Test doubles for the worker loop.

RedisJobStore plays the part of the jobs table. It lives in Redis (not in a
Python dict) because the crash test runs consumers in separate processes, and
they all need to see the same job rows.
"""

from __future__ import annotations

import json

import redis

from rigforge_worker.worker import JobRow


class RedisJobStore:
    def __init__(self, client: redis.Redis, prefix: str):
        self.r = client
        self.prefix = prefix

    def _key(self, job_id: str) -> str:
        return f"{self.prefix}:job:{job_id}"

    # ---- test setup / inspection ----

    def create(self, job_id: str, asset_id: str = "asset-1") -> None:
        self.r.hset(
            self._key(job_id), mapping={"asset_id": asset_id, "status": "queued", "attempt": 1, "history": "[]"}
        )

    def delete(self, job_id: str) -> None:
        self.r.delete(self._key(job_id))

    def row(self, job_id: str) -> dict:
        row = self.r.hgetall(self._key(job_id))
        row["history"] = json.loads(row.get("history", "[]"))
        return row

    def _update(self, job_id: str, event: str, **fields) -> None:
        key = self._key(job_id)
        history = json.loads(self.r.hget(key, "history") or "[]")
        history.append(event)
        self.r.hset(key, mapping={**fields, "history": json.dumps(history)})

    # ---- JobStore protocol ----

    def load(self, job_id: str) -> JobRow | None:
        row = self.r.hgetall(self._key(job_id))
        if not row:
            return None
        return JobRow(
            id=job_id,
            asset_id=row["asset_id"],
            status=row["status"],
            character_id="default",
            source_key="assets/x/source.mp4",
            error=row.get("error") or None,
        )

    def begin_attempt(self, job_id: str, attempt: int) -> None:
        self._update(job_id, f"begin:{attempt}", status="transcoding", attempt=attempt, progress=0)

    def set_stage(self, job_id: str, status: str, progress: int) -> None:
        self._update(job_id, f"stage:{status}:{progress}", status=status, progress=progress)

    def schedule_retry(self, job_id: str, next_attempt: int, retry_at: float, error: str) -> None:
        self._update(
            job_id, f"retry:{next_attempt}", status="queued", attempt=next_attempt, retry_at=retry_at, error=error
        )

    def fail(self, job_id: str, error: str) -> None:
        self._update(job_id, "fail", status="failed", error=error)

    def complete(self, job_id: str, metadata: dict) -> None:
        self._update(job_id, "complete", status="done", progress=100, error="", metadata=json.dumps(metadata))
