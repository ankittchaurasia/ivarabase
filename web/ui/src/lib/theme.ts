import { useEffect, useState } from "react";

export type ThemeMode = "light" | "dark" | "system";
const KEY = "ivara.theme";

function read(): ThemeMode {
  try {
    const v = localStorage.getItem(KEY);
    if (v === "light" || v === "dark" || v === "system") return v;
  } catch {}
  return "system";
}

const media = () => window.matchMedia("(prefers-color-scheme: dark)");

export function applyTheme(mode: ThemeMode = read()) {
  const dark = mode === "dark" || (mode === "system" && media().matches);
  document.documentElement.classList.toggle("dark", dark);
  return dark;
}

const listeners = new Set<() => void>();

export function useTheme() {
  const [mode, setModeState] = useState<ThemeMode>(read);
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));
  useEffect(() => {
    const sync = () => {
      setModeState(read());
      setDark(applyTheme());
    };
    listeners.add(sync);
    const m = media();
    m.addEventListener("change", sync);
    return () => {
      listeners.delete(sync);
      m.removeEventListener("change", sync);
    };
  }, []);
  const setMode = (m: ThemeMode) => {
    try {
      localStorage.setItem(KEY, m);
    } catch {}
    applyTheme(m);
    listeners.forEach((fn) => fn());
  };
  return { mode, dark, setMode };
}
