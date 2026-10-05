import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { ApiProvider } from "./api/context";
import { App } from "./App";
import { AuthProvider } from "./auth/auth";
import "./styles/global.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      // Presigned URLs in asset responses expire; refetching on focus keeps a
      // tab that was left open from showing dead links.
      refetchOnWindowFocus: true,
    },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <ApiProvider>
          <BrowserRouter>
            <App />
          </BrowserRouter>
        </ApiProvider>
      </AuthProvider>
    </QueryClientProvider>
  </StrictMode>,
);
