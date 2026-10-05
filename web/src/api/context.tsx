import { createContext, useContext, useMemo, useRef, type ReactNode } from "react";
import { useAuth } from "../auth/auth";
import { createApi, type Api } from "./client";

const API_URL = import.meta.env.VITE_API_URL ?? ""; // same origin by default

const ApiContext = createContext<Api | null>(null);

/**
 * Provides one api client for the app. The client reads the token through a
 * ref, so it is created once and always sends the current session's token;
 * a 401 from any request signs the user out.
 */
export function ApiProvider({ children, api }: { children: ReactNode; api?: Api }) {
  const { session, signOut } = useAuth();
  const token = useRef<string | null>(null);
  token.current = session?.token ?? null;

  const client = useMemo(
    () => api ?? createApi({ baseUrl: API_URL, getToken: () => token.current, onUnauthorized: signOut }),
    [api, signOut],
  );
  return <ApiContext.Provider value={client}>{children}</ApiContext.Provider>;
}

export function useApi(): Api {
  const api = useContext(ApiContext);
  if (!api) throw new Error("useApi must be used inside <ApiProvider>");
  return api;
}
