import { useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useApi } from "../api/context";
import type { AssetSort } from "../api/types";
import { AssetGrid } from "../components/AssetGrid";
import styles from "./BrowsePage.module.css";

const SORTS: { value: AssetSort; label: string }[] = [
  { value: "newest", label: "Newest first" },
  { value: "oldest", label: "Oldest first" },
  { value: "name", label: "Name" },
  { value: "duration", label: "Longest first" },
];

/** The value, but only after it has stopped changing for `ms`. */
function useDebounced<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(timer);
  }, [value, ms]);
  return settled;
}

export function BrowsePage() {
  const api = useApi();
  const [search, setSearch] = useState("");
  const [sort, setSort] = useState<AssetSort>("newest");
  // Search runs on the server; debounce so typing is not one request per key.
  const q = useDebounced(search.trim(), 250);

  const { data: assets, error, isPending } = useQuery({
    queryKey: ["assets", { q, sort }],
    queryFn: ({ signal }) => api.listAssets({ q, sort }, signal),
    // Poll only while something is still processing.
    refetchInterval: (query) => (query.state.data?.some((a) => a.status === "processing") ? 2000 : false),
    placeholderData: (previous) => previous, // keep the grid on screen while a new search loads
  });

  return (
    <>
      <div className={styles.header}>
        <div>
          <h1>Assets</h1>
          <p className="muted">Motion assets extracted from your videos.</p>
        </div>
        <Link to="/upload" className="btn btn-primary">
          New upload
        </Link>
      </div>

      <div className={styles.toolbar}>
        <label className="field" style={{ flex: 1 }}>
          <span>Search</span>
          <input
            className="input"
            type="search"
            placeholder="Search by name"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
          />
        </label>
        <label className="field">
          <span>Sort</span>
          <select className="select" value={sort} onChange={(e) => setSort(e.target.value as AssetSort)}>
            {SORTS.map((s) => (
              <option key={s.value} value={s.value}>
                {s.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      {error ? (
        <p className="notice notice-error" role="alert">
          Could not load assets: {error.message}
        </p>
      ) : isPending ? (
        <p className="muted">Loading…</p>
      ) : assets.length === 0 ? (
        <div className={`panel ${styles.empty}`}>
          {q ? (
            <p>No assets match “{q}”.</p>
          ) : (
            <>
              <p>No assets yet.</p>
              <p className="muted">Upload a video of one person moving and Rigforge turns it into a glTF motion asset.</p>
              <Link to="/upload" className="btn btn-primary">
                Upload a video
              </Link>
            </>
          )}
        </div>
      ) : (
        <AssetGrid assets={assets} />
      )}
    </>
  );
}
