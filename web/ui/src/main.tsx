import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
import "./index.css";
import { applyTheme } from "./lib/theme";

applyTheme(); // before first paint, so there is no light flash in dark mode

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode, useEffect, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes, useNavigate } from "react-router-dom";
import { Layout } from "./components/Layout";
import { ConfirmProvider, Toaster } from "./components/ui";
import { ApiError } from "./lib/api";
import { Account } from "./pages/Account";
import { Api } from "./pages/Api";
import { Approvals } from "./pages/Approvals";
import { Audit } from "./pages/Audit";
import { Auth } from "./pages/Auth";
import { Backups, ProjectBackups } from "./pages/Backups";
import { Dashboard } from "./pages/Dashboard";
import { Extensions } from "./pages/Extensions";
import { FunctionEditor, Functions } from "./pages/Functions";
import { Login } from "./pages/Login";
import { Migrations } from "./pages/Migrations";
import { Monitoring } from "./pages/Monitoring";
import { ProjectApi } from "./pages/ProjectApi";
import { ProjectOverview } from "./pages/ProjectOverview";
import { Projects } from "./pages/Projects";
import { Secrets } from "./pages/Secrets";
import { Storage } from "./pages/Storage";
import { SqlEditor } from "./pages/SqlEditor";
import { Tables } from "./pages/Tables";
import { Tokens } from "./pages/Tokens";
import { Webhooks } from "./pages/Webhooks";

const qc = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (n, e) => !(e instanceof ApiError && (e.status < 500 || e.code === "server_unreachable")) && n < 2,
      refetchOnWindowFocus: true,
      staleTime: 5_000,
    },
  },
});

function AuthWatcher({ children }: { children: ReactNode }) {
  const nav = useNavigate();
  useEffect(() => {
    const go = () => {
      if (location.pathname !== "/login") {
        qc.clear();
        nav("/login", { replace: true });
      }
    };
    window.addEventListener("ivara:unauthorized", go);
    return () => window.removeEventListener("ivara:unauthorized", go);
  }, [nav]);
  return <>{children}</>;
}

const shell = (el: ReactNode) => <Layout>{el}</Layout>;

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={qc}>
      <BrowserRouter>
        <ConfirmProvider>
          <AuthWatcher>
            <Routes>
              <Route path="/login" element={<Login />} />
              <Route path="/" element={shell(<Dashboard />)} />
              <Route path="/projects" element={shell(<Projects />)} />
              <Route path="/p/:slug" element={shell(<ProjectOverview />)} />
              <Route path="/p/:slug/api" element={shell(<ProjectApi />)} />
              <Route path="/p/:slug/auth" element={shell(<Auth />)} />
              <Route path="/p/:slug/storage" element={shell(<Storage />)} />
              <Route path="/p/:slug/tables" element={shell(<Tables />)} />
              <Route path="/p/:slug/tables/:table" element={shell(<Tables />)} />
              <Route path="/p/:slug/sql" element={shell(<SqlEditor />)} />
              <Route path="/p/:slug/migrations" element={shell(<Migrations />)} />
              <Route path="/p/:slug/extensions" element={shell(<Extensions />)} />
              <Route path="/p/:slug/functions" element={shell(<Functions />)} />
              <Route path="/p/:slug/functions/:name" element={shell(<FunctionEditor />)} />
              <Route path="/p/:slug/secrets" element={shell(<Secrets />)} />
              <Route path="/p/:slug/webhooks" element={shell(<Webhooks />)} />
              <Route path="/p/:slug/backups" element={shell(<ProjectBackups />)} />
              <Route path="/backups" element={shell(<Backups />)} />
              <Route path="/monitoring" element={shell(<Monitoring />)} />
              <Route path="/approvals" element={shell(<Approvals />)} />
              <Route path="/tokens" element={shell(<Tokens />)} />
              <Route path="/audit" element={shell(<Audit />)} />
              <Route path="/docs" element={shell(<Api />)} />
              <Route path="/api" element={<Navigate to="/docs" replace />} />
              <Route path="/account" element={shell(<Account />)} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </AuthWatcher>
        </ConfirmProvider>
        <Toaster />
      </BrowserRouter>
    </QueryClientProvider>
  </StrictMode>,
);
