# syntax=docker/dockerfile:1
#
# The Rigforge worker. Three things are pinned here because they decide what
# motion comes out for a given video: the ffmpeg build, the MediaPipe version
# (in pyproject.toml) and the pose model file (by checksum).

# A static ffmpeg/ffprobe build, pinned by version tag. Static binaries need
# nothing from the base image, so they are simply copied in below.
ARG FFMPEG_VERSION=8.1.1
FROM mwader/static-ffmpeg:${FFMPEG_VERSION} AS ffmpeg

# The pose model. ADD --checksum makes the build fail if the file Google
# serves ever differs from the one this project was tested with. --chmod
# because a file ADDed from a URL is otherwise created 0600, unreadable for
# the non-root user the worker runs as.
FROM scratch AS model
ADD --checksum=sha256:64437af838a65d18e5ba7a0d39b465540069bc8aae8308de3e318aad31fcbc7b --chmod=0644 \
    https://storage.googleapis.com/mediapipe-models/pose_landmarker/pose_landmarker_heavy/float16/1/pose_landmarker_heavy.task \
    /models/pose_landmarker_heavy.task

FROM python:3.11-slim-bookworm AS runtime
# Shared libraries MediaPipe and its OpenCV dependency load at import time.
RUN apt-get update && \
    apt-get install -y --no-install-recommends libgl1 libglib2.0-0 libegl1 libgles2 && \
    rm -rf /var/lib/apt/lists/*
COPY --from=ffmpeg /ffmpeg /ffprobe /usr/local/bin/

WORKDIR /app
COPY pyproject.toml ./
COPY rigforge_worker ./rigforge_worker
# The cache mount keeps downloaded wheels between builds without putting
# them in an image layer.
RUN --mount=type=cache,target=/root/.cache/pip pip install .
COPY rig ./rig
COPY --from=model /models /app/models

ENV POSE_MODEL_PATH=/app/models/pose_landmarker_heavy.task \
    RIGFORGE_RIG_DIR=/app/rig \
    RIGFORGE_WORK_DIR=/work \
    WORKER_ALIVE_FILE=/tmp/worker.alive \
    MPLCONFIGDIR=/tmp/matplotlib \
    PYTHONUNBUFFERED=1

# Run as an unprivileged user. /work holds per-job temp files (the source
# video and intermediates); on Kubernetes it is an emptyDir volume.
RUN useradd --system --uid 10001 --home-dir /tmp worker && mkdir /work && chown worker /work
USER 10001
ENTRYPOINT ["rigforge-worker"]
