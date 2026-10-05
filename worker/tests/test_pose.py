"""Gap filling and smoothing of the raw landmark track."""

from __future__ import annotations

import numpy as np
import pytest

from rigforge_worker import pose
from rigforge_worker.errors import PermanentError


def make_track(frames: int, missing: list[int]) -> pose.PoseTrack:
    """A track whose every landmark coordinate equals the frame number, so
    interpolated values are easy to predict. `missing` frames are undetected."""
    detected = np.ones(frames, dtype=bool)
    detected[missing] = False
    ramp = np.arange(frames, dtype=float)
    world = np.tile(ramp[:, None, None], (1, 33, 3))
    image = np.tile(ramp[:, None, None], (1, 33, 2))
    world[~detected] = np.nan
    image[~detected] = np.nan
    visibility = np.where(detected[:, None], 0.9, 0.0) * np.ones((frames, 33))
    return pose.PoseTrack(world, image, visibility, detected, width=720, height=720, fps=30.0)


def test_short_gap_is_interpolated():
    track = pose.fill_gaps(make_track(20, missing=[5, 6, 7]))

    # The landmark value is the frame index, so a correct linear interpolation
    # restores exactly the missing values.
    assert np.allclose(track.world[:, 0, 0], np.arange(20))
    assert np.allclose(track.image[:, 4, 1], np.arange(20))
    assert track.gaps == []
    # Filled frames are flagged as not actually seen.
    assert np.all(track.visibility[[5, 6, 7]] == 0.0)
    assert not np.isnan(track.world).any()


def test_nine_frames_are_interpolated_but_ten_are_a_gap():
    nine = pose.fill_gaps(make_track(40, missing=list(range(10, 19))))
    assert nine.gaps == []
    assert np.allclose(nine.world[:, 0, 0], np.arange(40))

    ten = pose.fill_gaps(make_track(40, missing=list(range(10, 20))))
    assert ten.gaps == [(10, 20)]
    # The last pose before the gap (frame 9) is held through it, not blended
    # towards frame 20: nothing is invented for a long loss of tracking.
    assert np.allclose(ten.world[10:20, 0, 0], 9.0)
    assert ten.world[20, 0, 0] == 20.0


def test_missing_frames_at_the_edges_take_the_nearest_pose():
    track = pose.fill_gaps(make_track(20, missing=[0, 1, 2, 18, 19]))
    assert np.allclose(track.world[:3, 0, 0], 3.0)
    assert np.allclose(track.world[18:, 0, 0], 17.0)
    assert track.gaps == []


def test_long_loss_at_the_start_is_recorded_as_a_gap():
    track = pose.fill_gaps(make_track(40, missing=list(range(0, 15))))
    assert track.gaps == [(0, 15)]
    assert np.allclose(track.world[:15, 0, 0], 15.0)  # holds the first pose seen


def test_no_detection_at_all_is_a_permanent_error():
    with pytest.raises(PermanentError, match="no person"):
        pose.fill_gaps(make_track(10, missing=list(range(10))))


def test_smoothing_removes_jitter_from_a_still_joint():
    rng = np.random.default_rng(0)
    noisy = 1.0 + rng.normal(scale=0.01, size=(300, 33, 3))  # 1 cm of jitter

    smoothed = pose.smooth(noisy, rate=30.0)

    assert smoothed.std() < noisy.std() / 2, "jitter should be at least halved"
    assert smoothed.mean() == pytest.approx(1.0, abs=1e-3)


def test_smoothing_does_not_lag_behind_fast_motion():
    # A joint sweeping 1 m back and forth twice a second.
    t = np.arange(300) / 30.0
    signal = np.sin(2 * np.pi * 2.0 * t)[:, None]

    smoothed = pose.smooth(signal, rate=30.0)

    # Zero lag: the smoothed curve peaks on the same frames as the input...
    interior = slice(30, 270)
    lag = np.argmax(np.correlate(smoothed[interior, 0], signal[interior, 0], mode="full")) - (240 - 1)
    assert lag == 0
    # ...and fast motion keeps most of its amplitude.
    assert np.abs(smoothed[interior]).max() > 0.85


def test_forward_only_filter_does_lag():
    """Shows why smooth() runs the filter in both directions."""
    t = np.arange(300) / 30.0
    signal = np.sin(2 * np.pi * 2.0 * t)[:, None]

    causal = pose.one_euro(signal, rate=30.0, min_cutoff=2.0, beta=1.0)

    interior = slice(30, 270)
    lag = np.argmax(np.correlate(causal[interior, 0], signal[interior, 0], mode="full")) - (240 - 1)
    assert lag > 0


def test_a_few_stray_detections_are_not_a_person():
    # The detector fires on 2 of 30 frames of footage with nobody in it.
    detected = np.zeros(30, dtype=bool)
    detected[[4, 17]] = True
    with pytest.raises(PermanentError, match="only 2 of 30 frames"):
        pose.require_person(detected)

    # Present for a third of a long clip: accepted.
    detected = np.zeros(300, dtype=bool)
    detected[100:200] = True
    pose.require_person(detected)

    # Present throughout a very short clip (under half a second): rejected.
    with pytest.raises(PermanentError):
        pose.require_person(np.ones(10, dtype=bool))
