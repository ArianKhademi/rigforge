"""The worker's SQL against the real schema.

The api owns the migrations; this test applies those same SQL files into a
throwaway Postgres schema and runs every PostgresJobStore method against it,
so a schema change that breaks the worker fails here.
"""

from __future__ import annotations

import os
import time
import uuid
from pathlib import Path

import psycopg
import pytest

from rigforge_worker.db import EVENT_CHANNEL_PREFIX, PostgresJobStore

from conftest import unavailable

pytestmark = pytest.mark.postgres

DATABASE_URL = os.environ.get("RIGFORGE_TEST_DATABASE_URL", "postgres://rigforge:rigforge@localhost:55432/rigforge")
MIGRATIONS = Path(__file__).resolve().parents[2] / "api" / "internal" / "store" / "migrations"


@pytest.fixture
def database():
    """A fresh schema with the api's migrations applied; dropped afterwards."""
    try:
        admin = psycopg.connect(DATABASE_URL, autocommit=True)
    except psycopg.OperationalError:
        unavailable(f"Postgres not reachable at {DATABASE_URL}; run `make infra`")
    schema = f"worker_test_{uuid.uuid4().hex[:10]}"
    admin.execute(f"CREATE SCHEMA {schema}")
    admin.execute(f"SET search_path TO {schema}")
    for path in sorted(MIGRATIONS.glob("*.sql")):
        admin.execute(path.read_text())
    # Connections opened by the store must land in the same schema.
    url = f"{DATABASE_URL}{'&' if '?' in DATABASE_URL else '?'}options=-csearch_path%3D{schema}"
    yield admin, url
    admin.execute(f"DROP SCHEMA {schema} CASCADE")
    admin.close()


def seed(admin) -> tuple[str, str]:
    asset_id, job_id = str(uuid.uuid4()), str(uuid.uuid4())
    admin.execute(
        "INSERT INTO assets (id, user_id, name, source_key, source_size, content_type, status) "
        "VALUES (%s, 'alice', 'Dance', %s, 123, 'video/mp4', 'processing')",
        (asset_id, f"assets/{asset_id}/source.mp4"),
    )
    admin.execute(
        "INSERT INTO jobs (id, asset_id, user_id, type, character_id, status) "
        "VALUES (%s, %s, 'alice', 'process_video', 'default', 'queued')",
        (job_id, asset_id),
    )
    return asset_id, job_id


def job(admin, job_id) -> dict:
    cur = admin.execute(
        "SELECT status, progress, attempt, error, retry_at, started_at, finished_at FROM jobs WHERE id = %s", (job_id,)
    )
    return dict(zip([c.name for c in cur.description], cur.fetchone(), strict=True))


def asset(admin, asset_id) -> dict:
    cur = admin.execute(
        "SELECT status, duration_seconds, width, height, fps, frame_count FROM assets WHERE id = %s", (asset_id,)
    )
    return dict(zip([c.name for c in cur.description], cur.fetchone(), strict=True))


def test_load_joins_the_asset(database):
    admin, url = database
    asset_id, job_id = seed(admin)
    store = PostgresJobStore(url)

    row = store.load(job_id)
    assert row.id == job_id and row.asset_id == asset_id
    assert (row.status, row.character_id, row.error) == ("queued", "default", None)
    assert row.source_key == f"assets/{asset_id}/source.mp4"
    assert store.load(str(uuid.uuid4())) is None


def test_successful_job_lifecycle(database):
    admin, url = database
    asset_id, job_id = seed(admin)
    store = PostgresJobStore(url)

    store.begin_attempt(job_id, 1)
    assert job(admin, job_id)["status"] == "transcoding"
    assert job(admin, job_id)["started_at"] is not None

    store.set_stage(job_id, "extracting", 40)
    assert (job(admin, job_id)["status"], job(admin, job_id)["progress"]) == ("extracting", 40)

    store.complete(job_id, {"duration_seconds": 5.2, "width": 720, "height": 720, "fps": 30.0, "frame_count": 156})
    done = job(admin, job_id)
    assert (done["status"], done["progress"], done["error"]) == ("done", 100, None)
    assert done["finished_at"] is not None
    # The asset becomes ready and gets its metadata in the same transaction.
    assert asset(admin, asset_id) == {
        "status": "ready", "duration_seconds": 5.2, "width": 720, "height": 720, "fps": 30.0, "frame_count": 156,
    }  # fmt: skip


def test_retry_then_failure(database):
    admin, url = database
    asset_id, job_id = seed(admin)
    store = PostgresJobStore(url)

    store.begin_attempt(job_id, 1)
    retry_at = time.time() + 60
    store.schedule_retry(job_id, 2, retry_at, "RuntimeError: bucket timeout")
    waiting = job(admin, job_id)
    assert (waiting["status"], waiting["attempt"], waiting["error"]) == ("queued", 2, "RuntimeError: bucket timeout")
    assert waiting["retry_at"].timestamp() == pytest.approx(retry_at, abs=0.01)
    assert asset(admin, asset_id)["status"] == "processing"  # still in flight

    # The retry starts: the wait is over but the last error stays visible.
    store.begin_attempt(job_id, 2)
    running = job(admin, job_id)
    assert (running["attempt"], running["retry_at"], running["error"]) == (2, None, "RuntimeError: bucket timeout")

    store.fail(job_id, "RuntimeError: bucket timeout")
    failed = job(admin, job_id)
    assert (failed["status"], failed["error"]) == ("failed", "RuntimeError: bucket timeout")
    assert failed["finished_at"] is not None
    assert asset(admin, asset_id)["status"] == "failed"


def test_progress_writes_are_throttled_within_a_stage(database):
    admin, url = database
    _, job_id = seed(admin)
    store = PostgresJobStore(url)
    store.begin_attempt(job_id, 1)

    for pct in range(1, 30):  # a burst of progress callbacks
        store.set_stage(job_id, "transcoding", pct)
    assert job(admin, job_id)["progress"] == 0, "updates inside the throttle window are dropped"

    # A stage change is always written, whatever the timing.
    store.set_stage(job_id, "extracting", 30)
    assert (job(admin, job_id)["status"], job(admin, job_id)["progress"]) == ("extracting", 30)


def test_set_stage_reports_a_deleted_job(database):
    admin, url = database
    asset_id, job_id = seed(admin)
    store = PostgresJobStore(url)
    store.begin_attempt(job_id, 1)
    assert store.set_stage(job_id, "extracting", 30) is True

    admin.execute("DELETE FROM assets WHERE id = %s", (asset_id,))  # cascades to the job

    assert store.set_stage(job_id, "writing", 85) is False


def test_store_reconnects_after_the_connection_drops(database):
    admin, url = database
    _, job_id = seed(admin)
    store = PostgresJobStore(url)
    assert store.load(job_id) is not None

    # Kill the store's connection from the server side (what a Postgres
    # restart looks like to the worker).
    admin.execute(
        "SELECT pg_terminate_backend(pid) FROM pg_stat_activity "
        "WHERE pid <> pg_backend_pid() AND query LIKE '%%FROM jobs j JOIN assets%%'"
    )
    assert store.load(job_id) is not None, "the store should reconnect transparently"


@pytest.mark.redis
def test_every_change_publishes_a_job_event(database, rdb):
    admin, url = database
    _, job_id = seed(admin)
    listener = rdb.pubsub()
    listener.subscribe(EVENT_CHANNEL_PREFIX + job_id)
    listener.get_message(timeout=1)  # the subscribe confirmation
    store = PostgresJobStore(url, events=rdb)

    store.begin_attempt(job_id, 1)
    store.set_stage(job_id, "extracting", 30)
    store.complete(job_id, {})

    received = []
    while (message := listener.get_message(timeout=0.5)) is not None:
        received.append(message["channel"])
    assert received == [EVENT_CHANNEL_PREFIX + job_id] * 3
    listener.close()
