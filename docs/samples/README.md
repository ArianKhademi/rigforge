# Sample clip

`power_jump.mp4` is the input used in the README, in the worker's smoke test
and in the end-to-end test: 5.2 seconds, 720x720, one person doing two power
jumps in front of a static camera.

## Source and licence

- Source: "Conditioning Drill 1: Power Jump", Army Combat Fitness Test (U.S. Army),
  from Wikimedia Commons:
  <https://commons.wikimedia.org/wiki/File:Conditioning_Drill_1-_Power_Jump.webm>
- Licence: **public domain**. It is a work of the U.S. federal government
  (Commons tag `PD-USGov-Military-Army`), so there are no copyright
  restrictions. Note this is public domain by law rather than a CC0 dedication;
  the practical effect (free to use, modify and redistribute) is the same.
- Use of the clip does not imply endorsement by the U.S. Army.

## How it was made

`scripts/make_sample_video.sh sample` downloads the original (verifying its
SHA-256), takes the last continuous wide shot (45.25 s to 50.45 s), crops the
centre 1080x1080 square, scales it to 720x720 and encodes H.264 without audio.
The original is a 50-second edit that cuts between wide shots and close-ups;
the sample is a single uncut wide shot so the performer is visible head to
feet in every frame.
