/**
 * One clock for the 3D motion and the source video.
 *
 * The worker gives both the same time base (frame i is at i / 30 s), so
 * keeping them in sync is only a matter of asking one of them for the time.
 * When the preview video is available it is the master: the motion is posed
 * at video.currentTime on every rendered frame. If the video cannot be played
 * the clock keeps time itself, so the motion preview still works.
 */
export class PlaybackClock {
  private video: HTMLVideoElement | null = null;
  private time = 0;
  private running = false;
  loop = true;

  constructor(public duration: number) {}

  /** Attach (or detach, with null) the video element that drives the clock. */
  attach(video: HTMLVideoElement | null): void {
    this.video = video;
    if (video) {
      video.loop = this.loop;
      video.currentTime = this.time;
      if (this.running) void video.play().catch(() => undefined);
    }
  }

  private get master(): HTMLVideoElement | null {
    // readyState >= 1: the video's metadata is loaded, so its clock is usable.
    return this.video && this.video.readyState >= 1 && !this.video.error ? this.video : null;
  }

  now(): number {
    // Mirror the video's time, so the internal clock can take over from the
    // same instant if the video stops being usable.
    if (this.master) this.time = this.master.currentTime;
    return this.time;
  }

  get playing(): boolean {
    return this.master ? !this.master.paused && !this.master.ended : this.running;
  }

  /** Advance the internal clock by `delta` seconds. A no-op while the video is the master. */
  tick(delta: number): void {
    if (this.master || !this.running) return;
    this.time += delta;
    if (this.time >= this.duration) {
      if (this.loop && this.duration > 0) {
        this.time %= this.duration;
      } else {
        this.time = this.duration;
        this.running = false;
      }
    }
  }

  play(): void {
    this.running = true;
    if (!this.master && this.time >= this.duration) this.time = 0;
    const video = this.video;
    if (video && !video.error) {
      if (video.ended) video.currentTime = 0;
      // Asked even if the video is still loading: it then starts as soon as
      // it can. play() rejects if the browser blocks it; the internal flag
      // still reflects what the user asked for.
      void video.play().catch(() => undefined);
    }
  }

  pause(): void {
    this.running = false;
    this.video?.pause();
  }

  seek(seconds: number): void {
    this.time = Math.min(Math.max(seconds, 0), this.duration);
    if (this.master) this.master.currentTime = this.time;
  }

  setLoop(loop: boolean): void {
    this.loop = loop;
    if (this.video) this.video.loop = loop;
  }
}
