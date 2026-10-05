import { Navigate, Route, Routes } from "react-router-dom";
import { RequireAuth } from "./auth/auth";
import { LoginPage } from "./auth/LoginPage";
import { Layout } from "./components/Layout";
import { AssetPage } from "./pages/AssetPage";
import { BrowsePage } from "./pages/BrowsePage";
import { UploadPage } from "./pages/UploadPage";

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route index element={<BrowsePage />} />
        <Route path="upload" element={<UploadPage />} />
        <Route path="assets/:id" element={<AssetPage tab="preview" />} />
        <Route path="assets/:id/export" element={<AssetPage tab="export" />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
