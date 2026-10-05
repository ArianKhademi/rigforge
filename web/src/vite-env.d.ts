/// <reference types="vite/client" />

interface ImportMetaEnv {
  /** Origin of the api; empty means same origin (the default). */
  readonly VITE_API_URL?: string;
  /** Base URL of the dev token issuer; defaults to /issuer. */
  readonly VITE_ISSUER_URL?: string;
}
