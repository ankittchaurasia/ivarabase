/** @type {import('tailwindcss').Config} */
const c = (v) => `hsl(var(--${v}) / <alpha-value>)`;
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        background: c("background"),
        foreground: c("foreground"),
        card: c("card"),
        sidebar: c("sidebar"),
        muted: { DEFAULT: c("muted"), foreground: c("muted-foreground") },
        border: c("border"),
        input: c("input"),
        ring: c("primary"),
        primary: { DEFAULT: c("primary"), foreground: c("primary-foreground") },
        accent: { DEFAULT: c("accent"), foreground: c("foreground") },
        destructive: { DEFAULT: c("destructive"), foreground: c("primary-foreground") },
        warning: c("warning"),
        success: c("success"),
        info: c("info"),
      },
      fontFamily: {
        sans: ['"Inter Variable"', "system-ui", "-apple-system", "Segoe UI", "sans-serif"],
        mono: ['"JetBrains Mono Variable"', "ui-monospace", "SFMono-Regular", "Menlo", "monospace"],
      },
      borderRadius: { lg: "0.75rem", md: "0.55rem", sm: "0.4rem" },
      boxShadow: {
        soft: "0 1px 2px rgb(0 0 0 / .04), 0 8px 24px -12px rgb(0 0 0 / .16)",
        pop: "0 12px 40px -12px rgb(0 0 0 / .35)",
      },
      keyframes: {
        "fade-in": { from: { opacity: 0, transform: "translateY(4px)" }, to: { opacity: 1, transform: "none" } },
      },
      animation: { "fade-in": "fade-in .16s ease-out" },
    },
  },
  plugins: [],
};
