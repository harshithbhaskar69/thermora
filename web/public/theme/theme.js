// Shared by the dashboard, explorer and separate manufacturer origin.
// Blocking, self-hosted script: select the palette before the first paint.
(() => {
  const root = document.documentElement;
  const media = window.matchMedia("(prefers-color-scheme: dark)");
  let preference = "system";
  function apply(value) {
    preference = value === "light" || value === "dark" ? value : "system";
    root.dataset.theme = preference;
    root.dataset.colorMode =
      preference === "system" ? (media.matches ? "dark" : "light") : preference;
    document.querySelectorAll("[data-theme-toggle]").forEach((button) => {
      const label = `Switch to ${root.dataset.colorMode === "dark" ? "light" : "dark"} theme`;
      button.setAttribute("aria-label", label);
      button.setAttribute("title", label);
    });
    window.dispatchEvent(new Event("thermora-theme"));
  }
  try {
    preference = localStorage.getItem("thermora-theme") || "system";
  } catch {
    /* Storage may be unavailable in private browsing. */
  }
  apply(preference);
  media.addEventListener("change", () => apply(preference));
  window.addEventListener("storage", (event) => {
    if (event.key === "thermora-theme" || event.key === null)
      apply(event.newValue);
  });
  document.addEventListener("DOMContentLoaded", () => apply(preference));
  document.addEventListener("click", (event) => {
    if (
      !(event.target instanceof Element) ||
      !event.target.closest("[data-theme-toggle]")
    )
      return;
    apply(root.dataset.colorMode === "dark" ? "light" : "dark");
    try {
      localStorage.setItem("thermora-theme", preference);
    } catch {
      /* The selection still applies for this page. */
    }
  });
})();
