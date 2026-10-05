# Rigforge architecture

This is the long-form companion to the README: how each part works, why it is built that way, and where to find it in the code.

- [Components](#components)
- [Data model](#data-model)
- [Upload protocol](#upload-protocol)
- [Queue semantics](#queue-semantics)
- [Worker pipeline](#worker-pipeline)
- [Retargeting](#retargeting)
- [Output files](#output-files)
- [Web client](#web-client)
- [Deployment](#deployment)
- [Decisions](#decisions)
- [Running natively](#running-natively)

## Components

| Component | Code | Role |
| --- | --- | --- |
| api | `api/cmd/api`, `api/internal/*` | Go / Gin. Auth, uploads, assets, jobs, characters. Stateless; two replicas. |
| dev issuer | `api/cmd/issuer` | Signs RS256 tokens and serves a JWKS, so the api's verification path is real. |
| worker | `worker/rigforge_worker` | Python. Consumes jobs, runs ffmpeg and MediaPipe, writes the outputs. |
| web | `web/src` | React 18 + TypeScript + Vite. Upload, browse, preview, export. |
| Postgres | | Uploads, parts, assets, jobs, characters. The source of truth for all state. |
| Redis | | The job stream, the delayed set, the dead-letter stream, pub/sub nudges. |
| Bucket | | Cloudflare R2 in production, MinIO locally. Source videos and outputs. |

Layout of the api:

```
api/internal/
  config/      environment -> Config
  auth/        JWT verification middleware
  httpx/       error envelope, request logging, CORS
  store/       Store interface, Postgres and in-memory implementations, migrations
  storage/     ObjectStore interface, S3 implementation, in-memory fake
  upload/      multipart upload endpoints, the pure part plan, the reaper
  job/         job creation, Redis producer, dispatcher, status + SSE
  asset/       browse, detail, export, reprocess, delete
  character/   GLB rig validation and upload
  server/      router assembly
  apitest/     the real router on fakes, for handler tests
  integration/ tests against real Postgres, Redis and S3
```

Handlers depend on two interfaces, `store.Store` and `storage.ObjectStore`. Unit tests swap in the in-memory versions; `store/storetest` runs one behavioural suite against both the fake and Postgres so the fake cannot drift.

## Data model

```
uploads       id, user_id, asset_id, filename, size, part_size, part_count,
              object_key, s3_upload_id, status (uploading|completed|aborted), timestamps
upload_parts  (upload_id, part_number) -> etag, recorded_at
assets        id, user_id, name, source_key, source_size, status (processing|ready|failed),
              duration_seconds, width, height, fps, frame_count
jobs          id, asset_id, user_id, type, character_id, status, progress, attempt,
              max_attempts, error, retry_at, enqueued_at, timestamps
characters    id, user_id, name, object_key, size
```

The api owns the schema and migrates on boot under a Postgres advisory lock (two replicas start at once). The worker only reads `jobs` joined to `assets` and updates those two tables; its SQL is tested against the api's migration files (`worker/tests/test_db.py`).

Object keys: everything for an asset lives under `assets/{assetId}/` (`source.<ext>`, `motion.glb`, `motion.bvh`, `preview.mp4`, `poster.jpg`), so deleting an asset is a prefix delete. The asset id is allocated when the upload is created, before the asset row exists.

## Upload protocol

Endpoints (`api/internal/upload/handler.go`):

| Call | Effect |
| --- | --- |
| `POST /api/uploads` `{filename, size, contentType}` | `CreateMultipartUpload` in the bucket, an `uploads` row; returns `{uploadId, partSize, partCount}` |
| `POST /api/uploads/{id}/parts` `{partNumbers}` | presigned `UploadPart` URLs, 15 minutes |
| `PUT /api/uploads/{id}/parts/{n}` `{etag}` | records the ETag (idempotent upsert) |
| `GET /api/uploads/{id}` | recorded parts; `?storage=true` adds the bucket's own `ListParts` |
| `POST /api/uploads/{id}/reconcile` | adopts parts that are in the bucket but unrecorded, returns the upload |
| `POST /api/uploads/{id}/complete` `{name, characterId}` | completes, verifies, creates asset and job, enqueues |
| `DELETE /api/uploads/{id}` | aborts the multipart upload |

**Part plan.** `upload/plan.go` is pure: part count is a ceiling division, every part is `partSize` except the last, and an upload may complete only when no part number in `1..partCount` is missing. 64 MiB keeps a 2 GiB file at 32 parts and bounds the cost of one lost part.

**Recording a part** is one SQL statement: a CTE bumps the upload's `updated_at` (the reaper's idle clock) only if the upload is still `uploading`, and the insert happens only if that row came back. Re-reporting the same ETag keeps the original `recorded_at`; a different ETag replaces it (last write wins, as in S3).

**ETags are not verified when recorded.** That would be a bucket request per part. The bucket checks every ETag at `CompleteMultipartUpload`, and a wrong one fails there with a 409.

**Complete** does, in order: check that no part is missing; `CompleteMultipartUpload`; `HeadObject` and compare the size with the declared size; one transaction that marks the upload completed and inserts the asset and its first job; `XADD`. Failure handling:

- the client retries after losing the response: the upload is already `completed`, the same ids are returned;
- the api died after the bucket completed but before the transaction: the retry gets `NoSuchUpload`, sees the object exists and continues;
- the api died after the transaction but before `XADD`: the dispatcher finds the row with `enqueued_at IS NULL`.

**Resume.** The client stores `{uploadId, completed parts}` under a fingerprint of the file (name, size, modified time). On start it calls reconcile for that upload id and takes the server's part list as the truth. Reconcile exists for one specific gap: a part whose bytes reached the bucket but whose ETag report never arrived. Without it the resumed client would send those bytes again.

**Reaper.** Unfinished multipart parts are invisible in a bucket listing but stored and billed. Every api replica aborts uploads idle for 24 hours; racing replicas are harmless because aborting twice is a no-op.

**The presigning client** uses the *public* endpoint, because the host name is part of the signature and the browser must reach that exact host. On R2 the public and internal endpoints are the same; with MinIO the api talks to `minio:9000` and the browser to `localhost:9000`.

## Queue semantics

Producer: `api/internal/job`. Consumer: `worker/rigforge_worker/queue.py` and `worker.py`.

The message is deliberately tiny: `{jobId, assetId, type, attempt}`. Everything else is read from the job row, so a message can never be stale.

Life of a message:

```
XADD (api)
  -> XREADGROUP (worker)           pending, owned by that consumer
       success                     row = done, then XACK + XDEL
       transient failure           row = queued/attempt+1/retry_at, then
                                   MULTI { ZADD jobs:delayed; XACK; XDEL }
                                   ... later, Lua: ZRANGEBYSCORE -> XADD -> ZREM
       third failure / permanent   row = failed, then MULTI { XADD jobs:dead; XACK; XDEL }
       worker is shutting down     MULTI { XADD copy; XACK; XDEL }   (handed straight back)
       worker dies                 stays pending; XAUTOCLAIM by another worker
                                   after 60 s without a heartbeat
```

- **Row first, then queue.** Every exit path updates Postgres before it changes the stream. If the worker dies in between, the message is redelivered and the guards at the top of `Worker.process` finish the job: a `done` row is acked, a `failed` row is moved to the dead-letter stream, a missing row (asset deleted) is dropped.
- **Heartbeats** are `XCLAIM` of the worker's own message with `JUSTID`, which resets the idle clock without changing owner or delivery count. They run on a thread because the handler blocks the main thread for the whole job.
- **XDEL after XACK** keeps the stream holding only outstanding jobs, so `XLEN jobs` is the queue depth.
- **At-least-once.** A job can run twice (a slow worker finishing just as its message is reclaimed), never zero times. Outputs are written to fixed keys, so a second run overwrites the first.
- **Deleted while running.** Each stage change is an `UPDATE`; zero rows updated means the asset was deleted, and the worker stops and drops the message. No outputs are written for an asset that no longer exists.

Tests (`worker/tests/test_queue.py`) run against a real Redis. The crash test starts a consumer in a separate process, waits until it is mid-job, sends `SIGKILL`, and asserts that a second process claims and finishes the job. A companion test shows that with heartbeats a job longer than the visibility timeout is *not* stolen, and fails if heartbeats are turned off.

**Live progress.** `GET /api/jobs/{id}/events` subscribes to the job's pub/sub channel *before* reading the row (so nothing is missed in between), sends the row, and re-reads it on every nudge and every five seconds. The stream ends when the job is done or failed. Because the payload is always read from Postgres, a lost or duplicated nudge cannot produce a wrong event, and no sticky sessions are needed.

## Worker pipeline

`worker/rigforge_worker/pipeline.py`, with ffmpeg in `media.py` and MediaPipe in `pose.py`.

- Nothing loads a whole video into memory: boto3 streams the download to disk, ffmpeg works file to file, and frames for pose extraction come through a pipe one at a time (`width * height * 3` bytes per read).
- One ffmpeg invocation decodes the source once and encodes both the working copy and the preview through a `split` filter. For a multi-gigabyte upload, decoding twice would dominate.
- The working copy fixes the frame rate at 30 fps. Preview, pose samples and animation keyframes all derive from it, so frame *i* is at *i / 30* seconds everywhere. That shared time base is what lets the UI sync video and motion with a single clock.
- MediaPipe runs on CPU deliberately: same behaviour on a laptop and in the Linux image, and the GPU delegate cannot start headless.
- A video where a person is found in fewer than 15 frames or under 20% of frames is rejected as a permanent error. The detector does occasionally fire on footage with nobody in it.
- Errors are classified in one place: `PermanentError` for bad input, anything else is transient and retried.

## Retargeting

`worker/rigforge_worker/retarget.py`. Conventions: quaternions are `[x, y, z, w]`; space is glTF's (metres, +Y up, the character faces +Z, +X is its left). MediaPipe's world landmarks (x right, y down, z away) convert by negating y and z.

For each joint the solver computes a world-space delta rotation `D[j]`: what takes the bone from its bind-pose direction to its observed direction. In the T-pose every `D` is the identity.

1. **Level the data.** MediaPipe reports in the camera's frame. On the frames where the performer is at full height, the line from the supporting ankle to the shoulders should be vertical; its median pitch is taken as the camera's tilt and rotated out.
2. **Torso from frames.** One direction cannot fix a rotation (twist about it is free), but the torso has two: the hip line and the hip-centre-to-shoulder-centre line give an orthonormal frame for the pelvis; the shoulder line and the same up vector give one for the chest. `D = frame_now * frame_bind^T`. The spine joint takes the halfway rotation.
3. **Head from the ears and eyes**, with its neutral pitch calibrated on the standing frames (MediaPipe places the eyes well above the ear line), faded towards "follows the chest" when face landmarks are not visible.
4. **Limbs by swing.** For a joint with parent `p`: if it did not bend, its bone would point along `D[p] * bind_direction`. The shortest-arc rotation from there to the observed direction is the bend, and `D[j] = bend * D[p]`. Shortest-arc means "no added twist", the right default when twist is unobservable. Hands and feet do the same from the wrist and ankle, falling back to the parent's rotation when their small landmarks are unreliable.
5. **Local rotations.** `W[j] = D[j] * B[j]` where `B` is the joint's bind world rotation, and `local[j] = W[parent]^-1 * W[j]`. For the canonical skeleton `B` is the identity. A character exported from Blender has a rotation on every bone, and the same formula reproduces its own rest rotations in the T-pose.
6. **Sign continuity.** `q` and `-q` are the same rotation, but linear interpolation between them goes the long way round, so keyframes are flipped to stay on the same side as their predecessor.
7. **Root motion.** World landmarks are hip-centred, so position comes from the image: sideways from where the hips are, in metres via the body's own size in pixels (median-filtered over three seconds); height from how far the lowest foot is above the floor line. The root is then placed so that the *character's* lowest foot point is that high. Feet that are down in the video are down on the character whatever its proportions, and a squat lowers the hips because the legs fold.

The tests build synthetic performers (`worker/tests/synth.py`): pose a skeleton with known rotations, generate the landmarks a camera would see, solve, compare. They cover the T-pose, single-joint bends, whole-body turns, different proportions, characters with arbitrary bone axes, a pitched camera, a biased head, a sidestep, a jump and a squat.

## Output files

**motion.glb** (`gltf.py`, hand-written): for a skeleton-only job, a scene of 17 joint nodes, a skin with inverse bind matrices, and one animation with 17 rotation samplers and a hips translation sampler sharing one time accessor. With a character, the character's GLB is loaded, the animation accessors are appended to its binary buffer, and its meshes, materials and skin are left untouched. The Khronos validator reports no errors or warnings on either.

**motion.bvh** (`bvh.py`): offsets in centimetres, ZXY Euler channels, End Sites for the five leaf joints. The tests parse the file and evaluate it with BVH's own composition rule, then compare with forward kinematics.

**default_character.glb** is built by `worker/rig/make_default_character.py` with Blender's Python API: an armature read from the skeleton JSON and a mannequin of capsules, ellipsoids and joint spheres, each vertex bound rigidly to one bone.

## Web client

- `web/src/upload/state.ts`: the upload state machine as a pure reducer.
- `web/src/upload/uploader.ts`: the engine. A pool of workers pulls part numbers from one queue; presigned URLs are requested in batches and refreshed before they expire; retries back off exponentially with jitter; each run owns its `AbortController`.
- `web/src/api/sse.ts`: server-sent events over `fetch`, so the token is a header.
- `web/src/lib/clock.ts`: one clock for motion and video. The video's `currentTime` is the master when it can play; otherwise the clock runs itself.
- `web/src/components/MotionViewer.tsx`: react-three-fiber. The animation mixer never free-runs; every frame it is set to the clock's time.

## Deployment

`deploy/k8s/base` plus overlays, applied with kustomize.

- The api and web sit behind one Ingress, so the browser sees one origin. The bucket is a second origin; that is true for R2 as well, which is why the bucket needs a CORS policy that exposes `ETag` (`docs/r2-cors.json`).
- ConfigMap and Secret are *generated*, which appends a content hash to their names: changing a value rolls the pods that use it.
- The worker's liveness probe checks the age of a file it touches every loop iteration and on every heartbeat. A hung worker is restarted; a busy one is not.
- Workers wait for Redis at start-up instead of crash-looping, and the api waits for Postgres.
- On `SIGTERM` a worker finishes its current job (120 s grace) and hands back a message it had only just received.

Images: the Go binaries are static, on distroless. The worker is `python:3.11-slim` with a static ffmpeg copied from a pinned image and the pose model added by checksum. The web image is the built files behind unprivileged nginx.

## Decisions

Open choices in the brief and how they were settled.

| Choice | Decision | Why |
| --- | --- | --- |
| Postgres or SQLite | Postgres (pgx) | Two api replicas and two worker pods write concurrently from different pods; SQLite would need one shared file on one node. |
| JWT library | `lestrrat-go/jwx` v3 | The brief preferred a `jwkit` module if it existed; that folder is empty, so the stated fallback was used. |
| Queue | Redis Streams, own consumer | Retries, acks and dead-lettering are then explicit and explainable (the brief's stated preference). |
| Attempts and backoff | 3 attempts; schedule 1 min, 5 min, 15 min | The brief gives both "after 3 failed attempts" and a three-step schedule. Three attempts use the first two delays; the third applies if `JOB_MAX_ATTEMPTS` is raised. |
| Permanent errors | Dead-lettered on the first attempt | Retrying a file that is not a video only delays the error message by six minutes. |
| glTF writer | Hand-written | The job is narrow (one skin, one animation) and every byte stays explainable. |
| Pose model | `pose_landmarker_heavy` | Most accurate of the three; about 23 ms per frame on the test machine. |
| MediaPipe version | 1.0.0 | 1.0.1 aborts at graph start on macOS. |
| MinIO image | `ghcr.io/coollabsio/minio`, pinned | MinIO stopped publishing official images; this is a community build of the last open-source release. |
| Ingress controller in kind | Traefik | ingress-nginx is retired; Traefik needs no CRDs for plain Ingress resources. |
| Sample clip | U.S. Army fitness video (public domain) | A clean single-person, static-camera clip. Public domain as a U.S. government work rather than CC0; see `docs/samples/README.md`. |
| Character upload path | Through the api, not presigned | The api must parse the GLB to validate the rig; characters are capped at 32 MiB. |
| Extra endpoint: reconcile | Added | Closes the "PUT succeeded, report lost" gap so resume never re-sends a stored part. |

## Running natively

```bash
make setup            # worker venv, web deps, pose model
make infra            # postgres, redis, minio in Docker

# in three terminals, each with the environment below
(cd api && go run ./cmd/issuer)
(cd api && go run ./cmd/api)
(cd worker && .venv/bin/python -m rigforge_worker.main)

(cd web && pnpm dev)  # http://localhost:5173, proxies /api and /issuer
```

Environment for the api and worker when run this way:

```bash
export DATABASE_URL=postgres://rigforge:rigforge@localhost:55432/rigforge
export REDIS_URL=redis://localhost:56379/0
export S3_ENDPOINT=http://localhost:9000
export S3_ACCESS_KEY_ID=rigforge S3_SECRET_ACCESS_KEY=rigforge-dev-secret
export S3_BUCKET=rigforge-dev S3_CREATE_BUCKET=true
export JWT_JWKS_URL=http://localhost:8090/.well-known/jwks.json
```

The worker can also process a file with no services at all, which is how the sample output and the timings in the README were produced:

```bash
cd worker && .venv/bin/python -m rigforge_worker.main run ../docs/samples/power_jump.mp4 --out /tmp/out
```
