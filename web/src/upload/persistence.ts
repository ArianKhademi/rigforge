import type { PersistedUpload, ResumeStore } from "./uploader";

const PREFIX = "rigforge.upload.";

/**
 * Remembers in-flight uploads in localStorage, keyed by a fingerprint of the
 * file (name, size, modified time). After a reload or a crash the user picks
 * the same file again, the fingerprint matches, and the upload resumes.
 *
 * Only the upload id is essential. The completed-part list stored next to it
 * is a hint for the UI ("12 of 34 parts uploaded"); what actually gets sent on
 * resume is decided by the server's part list.
 */
export function localStorageStore(storage: Storage = window.localStorage): ResumeStore {
  return {
    load(key) {
      try {
        const raw = storage.getItem(PREFIX + key);
        return raw ? (JSON.parse(raw) as PersistedUpload) : null;
      } catch {
        return null; // unreadable entry: behave as if nothing was saved
      }
    },
    save(key, value) {
      try {
        storage.setItem(PREFIX + key, JSON.stringify(value));
      } catch {
        // Storage full or disabled: the upload still works, it just cannot
        // be resumed after a reload.
      }
    },
    remove(key) {
      storage.removeItem(PREFIX + key);
    },
  };
}

/** Uploads that were started on this device and never finished. */
export function listUnfinished(storage: Storage = window.localStorage): PersistedUpload[] {
  const found: PersistedUpload[] = [];
  for (let i = 0; i < storage.length; i++) {
    const key = storage.key(i);
    if (!key?.startsWith(PREFIX)) continue;
    try {
      found.push(JSON.parse(storage.getItem(key)!) as PersistedUpload);
    } catch {
      // ignore corrupt entries
    }
  }
  return found.sort((a, b) => b.updatedAt - a.updatedAt);
}

/** Forget a saved upload (the user chose to discard it). */
export function forgetUpload(uploadId: string, storage: Storage = window.localStorage): void {
  for (let i = storage.length - 1; i >= 0; i--) {
    const key = storage.key(i);
    if (!key?.startsWith(PREFIX)) continue;
    try {
      if ((JSON.parse(storage.getItem(key)!) as PersistedUpload).uploadId === uploadId) storage.removeItem(key);
    } catch {
      storage.removeItem(key);
    }
  }
}

/** In-memory store for tests. */
export function memoryStore(): ResumeStore & { entries: Map<string, PersistedUpload> } {
  const entries = new Map<string, PersistedUpload>();
  return {
    entries,
    load: (key) => entries.get(key) ?? null,
    save: (key, value) => void entries.set(key, structuredClone(value)),
    remove: (key) => void entries.delete(key),
  };
}
