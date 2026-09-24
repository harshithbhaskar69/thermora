import { useSyncExternalStore } from "react";

function subscribe(change: () => void) {
  window.addEventListener("thermora-theme", change);
  return () => window.removeEventListener("thermora-theme", change);
}

export function useColorMode() {
  return useSyncExternalStore(
    subscribe,
    () => document.documentElement.dataset.colorMode ?? "light",
  );
}

export function ThemeToggle() {
  const label = `Switch to ${useColorMode() === "dark" ? "light" : "dark"} theme`;
  return (
    <button
      type="button"
      className="theme-toggle"
      data-theme-toggle
      aria-label={label}
      title={label}
    >
      <svg
        aria-hidden="true"
        width="20"
        height="20"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.75"
        strokeLinecap="round"
        strokeLinejoin="round"
      >
        <g className="theme-sun">
          <circle cx="12" cy="12" r="4" />
          <path d="M12 2v2m0 16v2M2 12h2m16 0h2M5 5l1.5 1.5m11 11L19 19M5 19l1.5-1.5m11-11L19 5" />
        </g>
        <path
          className="theme-moon"
          d="M20.5 13.5A8.5 8.5 0 0 1 10.5 3.5a8.5 8.5 0 1 0 10 10Z"
        />
      </svg>
    </button>
  );
}
