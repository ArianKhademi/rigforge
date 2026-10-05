from __future__ import annotations

import os
import sys
import uuid
from pathlib import Path

import pytest
import redis

sys.path.insert(0, str(Path(__file__).parent))

REDIS_URL = os.environ.get("RIGFORGE_TEST_REDIS_URL", "redis://localhost:56379/14")
# CI sets this so a missing service fails the build instead of skipping tests.
REQUIRE_SERVICES = os.environ.get("RIGFORGE_REQUIRE_SERVICES") == "1"


def unavailable(reason: str):
    if REQUIRE_SERVICES:
        pytest.fail(reason)
    pytest.skip(reason)


@pytest.fixture
def redis_url() -> str:
    return REDIS_URL


@pytest.fixture
def rdb(redis_url):
    """A real Redis connection (docker-compose `redis` service)."""
    client = redis.Redis.from_url(redis_url, decode_responses=True)
    try:
        client.ping()
    except redis.ConnectionError:
        unavailable(f"Redis not reachable at {redis_url}; run `make infra`")
    yield client
    client.close()


@pytest.fixture
def prefix(rdb):
    """A unique key prefix per test, cleaned up afterwards, so tests cannot
    see each other's streams."""
    name = f"test:{uuid.uuid4().hex[:12]}"
    yield name
    for key in rdb.scan_iter(f"{name}:*"):
        rdb.delete(key)
