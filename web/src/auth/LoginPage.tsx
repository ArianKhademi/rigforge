import { useState, type FormEvent } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { useAuth } from "./auth";
import styles from "./LoginPage.module.css";

export function LoginPage() {
  const { session, signIn } = useAuth();
  const location = useLocation();
  const [user, setUser] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (session) {
    const from = (location.state as { from?: string } | null)?.from ?? "/";
    return <Navigate to={from} replace />;
  }

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await signIn(user.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : "Sign-in failed");
      setBusy(false);
    }
  };

  return (
    <div className={styles.page}>
      <form className={`panel ${styles.card}`} onSubmit={submit}>
        <div className={styles.brand}>
          <svg viewBox="0 0 32 32" width="34" height="34" aria-hidden="true">
            <g fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
              <path d="M16 12v8M16 14l-6 3M16 14l6-4M16 20l-4 6M16 20l4 6" />
            </g>
            <circle cx="16" cy="8" r="2.6" fill="currentColor" />
          </svg>
          <h1>Rigforge</h1>
        </div>
        <p className="muted">Turn a video of a person moving into a glTF motion asset.</p>

        <label className="field">
          <span>User name</span>
          <input
            className="input"
            value={user}
            onChange={(e) => setUser(e.target.value)}
            placeholder="e.g. arian"
            autoFocus
            required
            pattern="[a-zA-Z0-9_.@\-]{1,64}"
            title="1 to 64 letters, digits, or _ . @ -"
            autoComplete="username"
          />
        </label>
        {error && (
          <p className="notice notice-error" role="alert">
            {error}
          </p>
        )}
        <button className="btn btn-primary" disabled={busy || user.trim() === ""}>
          {busy ? "Signing in…" : "Sign in"}
        </button>
        <p className={styles.note}>
          Development sign-in. The bundled issuer signs an RS256 token for any name; the api verifies it against the
          issuer's JWKS and scopes every asset to that name.
        </p>
      </form>
    </div>
  );
}
