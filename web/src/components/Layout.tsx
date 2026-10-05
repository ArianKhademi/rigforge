import { NavLink, Outlet } from "react-router-dom";
import { useAuth } from "../auth/auth";
import styles from "./Layout.module.css";

export function Layout() {
  const { session, signOut } = useAuth();
  const link = ({ isActive }: { isActive: boolean }) => (isActive ? `${styles.link} ${styles.active}` : styles.link);
  return (
    <div className={styles.shell}>
      <header className={styles.header}>
        <div className={styles.bar}>
          <NavLink to="/" className={styles.brand} aria-label="Rigforge home">
            <svg viewBox="0 0 32 32" width="26" height="26" aria-hidden="true">
              <g fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" strokeLinejoin="round">
                <path d="M16 12v8M16 14l-6 3M16 14l6-4M16 20l-4 6M16 20l4 6" />
              </g>
              <circle cx="16" cy="8" r="2.6" fill="currentColor" />
            </svg>
            <span>Rigforge</span>
          </NavLink>
          <nav className={styles.nav} aria-label="Main">
            <NavLink to="/" end className={link}>
              Assets
            </NavLink>
            <NavLink to="/upload" className={link}>
              Upload
            </NavLink>
          </nav>
          <div className={styles.user}>
            <span className={styles.userName} title={session?.user}>
              {session?.user}
            </span>
            <button className="btn btn-ghost" onClick={signOut}>
              Sign out
            </button>
          </div>
        </div>
      </header>
      <main className={styles.main}>
        <Outlet />
      </main>
    </div>
  );
}
