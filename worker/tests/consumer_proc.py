"""A worker process for the crash-and-reclaim tests.

Usage: python consumer_proc.py <redis_url> <prefix> <consumer_name> <mode> <visibility_ms>

It runs the real Worker loop against the test's queue. The handler records
which consumer started and finished each job, then behaves according to mode:
    hang   start the job and never finish (the test then SIGKILLs the process)
    slow   take 3 seconds, heartbeating, then finish
    ok     finish immediately
"""

from __future__ import annotations

import sys
import time

import redis

from rigforge_worker.queue import JobQueue, QueueConfig
from rigforge_worker.worker import Worker

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from fakes import RedisJobStore  # noqa: E402


def main() -> None:
    url, prefix, name, mode, visibility_ms = sys.argv[1:6]
    client = redis.Redis.from_url(url, decode_responses=True)
    config = QueueConfig(
        stream=f"{prefix}:jobs",
        delayed_key=f"{prefix}:delayed",
        dead_stream=f"{prefix}:dead",
        visibility_timeout_ms=int(visibility_ms),
        block_ms=100,
    )
    store = RedisJobStore(client, prefix)

    def handler(job, progress):
        client.rpush(f"{prefix}:started:{job.id}", name)
        if mode == "hang":
            time.sleep(3600)
        if mode == "slow":
            time.sleep(3.0)
        client.rpush(f"{prefix}:finished:{job.id}", name)
        return {"processed_by": name}

    # A short heartbeat interval so "slow" proves heartbeats keep ownership.
    worker = Worker(JobQueue(client, name, config), store, handler, heartbeat_interval=0.2)
    client.set(f"{prefix}:ready:{name}", 1)
    worker.run_forever()


if __name__ == "__main__":
    main()
