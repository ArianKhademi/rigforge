import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";

// The session is the JWT the dev issuer handed out. It lives in localStorage
// so a reload (for example in the middle of an upload) does not sign the
// user out. The api verifies the token on every request; nothing here is
// trusted by the server.

const STORAGE_KEY = "rigforge.session";
const ISSUER_URL = import.meta.env.VITE_ISSUER_URL ?? "/issuer";

export interface Session {
  token: string;
  user: string;
  expiresAt: number; // ms since epoch
}

interface AuthValue {
  session: Session | null;
  signIn: (user: string) => Promise<void>;
  signOut: () => void;
}

const AuthContext = createContext<AuthValue | null>(null);

function loadSession(): Session | null {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const session = JSON.parse(raw) as Session;
    // An expired token would only earn a 401; treat it as signed out.
    return session.expiresAt > Date.now() ? session : null;
  } catch {
    return null;
  }
}

export function AuthProvider({ children, initial }: { children: ReactNode; initial?: Session | null }) {
  const [session, setSession] = useState<Session | null>(() => (initial !== undefined ? initial : loadSession()));

  const signIn = useCallback(async (user: string) => {
    const response = await fetch(`${ISSUER_URL}/token`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ sub: user }),
    });
    if (!response.ok) {
      const body = (await response.json().catch(() => null)) as { error?: string } | null;
      throw new Error(body?.error ?? `The issuer returned ${response.status}`);
    }
    const { access_token, expires_in } = (await response.json()) as { access_token: string; expires_in: number };
    const next: Session = { token: access_token, user, expiresAt: Date.now() + expires_in * 1000 };
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(next));
    setSession(next);
  }, []);

  const signOut = useCallback(() => {
    window.localStorage.removeItem(STORAGE_KEY);
    setSession(null);
  }, []);

  const value = useMemo(() => ({ session, signIn, signOut }), [session, signIn, signOut]);
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used inside <AuthProvider>");
  return value;
}

/** Renders its children only when signed in; otherwise redirects to /login. */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { session } = useAuth();
  const location = useLocation();
  if (!session) return <Navigate to="/login" replace state={{ from: location.pathname }} />;
  return <>{children}</>;
}
