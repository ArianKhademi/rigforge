"""Job queue on Redis Streams.

Why Streams and not a list or Celery: a consumer group gives every message an
owner and a pending-entries list (PEL), so "which worker has this job and for
how long" is state Redis keeps for us. Everything else (retries, backoff,
dead-lettering, reclaiming) is the ~200 lines below and fully explainable.

Keys
    jobs           stream; the api XADDs {jobId, assetId, type, attempt}
    workers        consumer group on that stream
    jobs:delayed   sorted set of retries waiting out their backoff, scored by
                   the unix time they become due
    jobs:dead      stream of jobs that will not be retried, with the last error

Life of a message
    XADD (api) -> XREADGROUP (worker; message is now pending, owned by it)
      success            -> XACK + XDEL
      transient failure  -> ZADD jobs:delayed + XACK + XDEL, later promoted
                            back onto the stream with attempt + 1
      attempts exhausted, or a permanent failure
                         -> XADD jobs:dead + XACK + XDEL
      worker dies        -> message stays pending; once it has been idle longer
                            than the visibility timeout another worker takes it
                            over with XAUTOCLAIM

Delivery is at-least-once: a job can run twice (a worker finishing just as its
message is reclaimed), never zero times. The pipeline is idempotent per job id,
so a duplicate run only overwrites the same output files.
"""

from __future__ import annotations

import json
import time
from dataclasses import dataclass, field

import redis


@dataclass(frozen=True)
class QueueConfig:
    stream: str = "jobs"
    group: str = "workers"
    delayed_key: str = "jobs:delayed"
    dead_stream: str = "jobs:dead"
    # A job is dead-lettered after this many failed attempts.
    max_attempts: int = 3
    # Seconds to wait after failed attempt 1, 2, 3, ... With max_attempts = 3
    # only the first two delays are ever used; the third applies if
    # max_attempts is raised.
    backoff_seconds: tuple[float, ...] = (60, 300, 900)
    # A pending message whose owner has not heartbeated for this long is
    # considered abandoned and may be claimed by another worker.
    visibility_timeout_ms: int = 60_000
    # How long XREADGROUP blocks waiting for a new message.
    block_ms: int = 2_000
    # A message that has been delivered more often than this keeps killing
    # workers before they can report a failure (e.g. out-of-memory). It is
    # dead-lettered instead of being reclaimed forever.
    max_deliveries: int = 5


@dataclass(frozen=True)
class Message:
    id: str  # stream entry id, e.g. "1717171717171-0"
    job_id: str
    asset_id: str
    type: str
    attempt: int  # 1-based
    reclaimed: bool = False  # True if taken over from another consumer
    fields: dict[str, str] = field(default_factory=dict, compare=False)


@dataclass(frozen=True)
class RetryDecision:
    dead: bool  # True: moved to the dead-letter stream, no more attempts
    next_attempt: int = 0
    retry_at: float = 0.0  # unix time the retry becomes due


# Atomically move due retries from the delayed set onto the stream. A Lua
# script runs as one uninterruptible unit in Redis, so with several workers
# calling it concurrently each retry is promoted exactly once, and a retry can
# never be removed from the set without also being added to the stream.
_PROMOTE_LUA = """
local due = redis.call('ZRANGEBYSCORE', KEYS[1], '-inf', ARGV[1], 'LIMIT', 0, 100)
for _, member in ipairs(due) do
    local job = cjson.decode(member)
    redis.call('XADD', KEYS[2], '*',
        'jobId', job.jobId, 'assetId', job.assetId, 'type', job.type, 'attempt', job.attempt)
    redis.call('ZREM', KEYS[1], member)
end
return #due
"""


class JobQueue:
    def __init__(self, client: redis.Redis, consumer: str, config: QueueConfig | None = None):
        """client must be created with decode_responses=True."""
        self.r = client
        self.consumer = consumer
        self.cfg = config or QueueConfig()
        self._promote = self.r.register_script(_PROMOTE_LUA)

    # ---- setup and producing ----

    def ensure_group(self) -> None:
        """Create the stream and consumer group if they do not exist.

        The group starts at id 0, not $, so jobs enqueued before the first
        worker ever started are still delivered.
        """
        try:
            self.r.xgroup_create(self.cfg.stream, self.cfg.group, id="0", mkstream=True)
        except redis.ResponseError as exc:
            if "BUSYGROUP" not in str(exc):  # BUSYGROUP = already exists
                raise

    def enqueue(self, job_id: str, asset_id: str, job_type: str = "process_video", attempt: int = 1) -> str:
        """Add a job. The api does this in production; tests use it directly."""
        return self.r.xadd(
            self.cfg.stream,
            {"jobId": job_id, "assetId": asset_id, "type": job_type, "attempt": str(attempt)},
        )

    # ---- consuming ----

    def next(self) -> Message | None:
        """Return the next job for this consumer, or None if nothing arrived
        within block_ms."""
        self.promote_due()
        # Abandoned work first: it has already waited a full visibility timeout.
        reclaimed = self._reclaim()
        if reclaimed is not None:
            return reclaimed
        # ">" means "messages never delivered to any consumer in the group".
        reply = self.r.xreadgroup(
            self.cfg.group, self.consumer, {self.cfg.stream: ">"}, count=1, block=self.cfg.block_ms
        )
        if not reply:
            return None
        _stream, entries = reply[0]
        entry_id, fields = entries[0]
        return self._message(entry_id, fields, reclaimed=False)

    def _reclaim(self) -> Message | None:
        """Take over one pending message whose owner stopped heartbeating."""
        # XAUTOCLAIM scans the group's pending list for entries idle for at
        # least min_idle_time, transfers ownership to this consumer and resets
        # their idle clock, all atomically, so two workers cannot both win.
        reply = self.r.xautoclaim(
            self.cfg.stream,
            self.cfg.group,
            self.consumer,
            min_idle_time=self.cfg.visibility_timeout_ms,
            start_id="0-0",
            count=1,
        )
        entries = reply[1]
        if not entries:
            return None
        entry_id, fields = entries[0]
        if fields is None:
            # The entry was deleted from the stream but is still in the pending
            # list; nothing to process, just clear it.
            self.r.xack(self.cfg.stream, self.cfg.group, entry_id)
            return None
        msg = self._message(entry_id, fields, reclaimed=True)

        # Redis counts deliveries per pending entry. A message that has gone
        # through many owners is a poison pill; stop the cycle.
        pending = self.r.xpending_range(self.cfg.stream, self.cfg.group, entry_id, entry_id, 1)
        deliveries = pending[0]["times_delivered"] if pending else 1
        if deliveries > self.cfg.max_deliveries:
            self.dead_letter(msg, f"abandoned by {deliveries - 1} workers without a result (crash loop)")
            return None
        return msg

    def _message(self, entry_id: str, fields: dict[str, str], reclaimed: bool) -> Message:
        return Message(
            id=entry_id,
            job_id=fields.get("jobId", ""),
            asset_id=fields.get("assetId", ""),
            type=fields.get("type", ""),
            attempt=int(fields.get("attempt", "1")),
            reclaimed=reclaimed,
            fields=fields,
        )

    def heartbeat(self, msg: Message) -> None:
        """Tell Redis this consumer is still working on msg.

        XCLAIM-ing our own message with min-idle-time 0 resets its idle clock
        without changing owner; JUSTID avoids incrementing the delivery count.
        A worker heartbeats every few seconds, so a long video never looks
        abandoned while a crashed worker's job does after visibility_timeout.
        """
        self.r.xclaim(
            self.cfg.stream, self.cfg.group, self.consumer, min_idle_time=0, message_ids=[msg.id], justid=True
        )

    # ---- finishing a message: every exit path acks and deletes it ----

    def ack(self, msg: Message) -> None:
        """Success. XDEL after XACK keeps the stream holding only outstanding
        jobs, which makes XLEN a usable queue-depth metric."""
        pipe = self.r.pipeline(transaction=True)
        pipe.xack(self.cfg.stream, self.cfg.group, msg.id)
        pipe.xdel(self.cfg.stream, msg.id)
        pipe.execute()

    def retry(self, msg: Message, error: str, now: float | None = None) -> RetryDecision:
        """Handle a failed attempt: schedule the next one, or dead-letter if
        the attempt budget is spent."""
        if msg.attempt >= self.cfg.max_attempts:
            self.dead_letter(msg, error)
            return RetryDecision(dead=True)

        now = time.time() if now is None else now
        schedule = self.cfg.backoff_seconds
        delay = schedule[min(msg.attempt - 1, len(schedule) - 1)]
        retry_at = now + delay
        member = json.dumps(
            {"jobId": msg.job_id, "assetId": msg.asset_id, "type": msg.type, "attempt": str(msg.attempt + 1)},
            sort_keys=True,
        )
        # MULTI/EXEC: the retry is scheduled and the failed delivery removed in
        # one transaction, so the job cannot be lost or doubled between the two.
        pipe = self.r.pipeline(transaction=True)
        pipe.zadd(self.cfg.delayed_key, {member: retry_at})
        pipe.xack(self.cfg.stream, self.cfg.group, msg.id)
        pipe.xdel(self.cfg.stream, msg.id)
        pipe.execute()
        return RetryDecision(dead=False, next_attempt=msg.attempt + 1, retry_at=retry_at)

    def dead_letter(self, msg: Message, error: str) -> None:
        """Park the job on jobs:dead with its last error for inspection."""
        pipe = self.r.pipeline(transaction=True)
        pipe.xadd(
            self.cfg.dead_stream,
            {
                "jobId": msg.job_id,
                "assetId": msg.asset_id,
                "type": msg.type,
                "attempt": str(msg.attempt),
                "error": error[:2000],
                "failedAt": f"{time.time():.3f}",
                "consumer": self.consumer,
            },
        )
        pipe.xack(self.cfg.stream, self.cfg.group, msg.id)
        pipe.xdel(self.cfg.stream, msg.id)
        pipe.execute()

    def promote_due(self, now: float | None = None) -> int:
        """Move retries whose backoff has elapsed back onto the stream."""
        now = time.time() if now is None else now
        return int(self._promote(keys=[self.cfg.delayed_key, self.cfg.stream], args=[now]))
