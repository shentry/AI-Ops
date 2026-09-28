import { Suspense, lazy, useEffect, useMemo, useState } from "react";
import { Route, Routes } from "react-router";

import { ModelState, Session, getCurrentModel, getSession, logout, unauthorizedEvent } from "../api";
import { AppLayout } from "../components/layout/AppLayout";
import { BrandMark } from "../components/layout/BrandMark";
import { Changes } from "../pages/Changes";
import { IncidentDetailRoute } from "../pages/IncidentDetail";
import { Incidents } from "../pages/Incidents";
import { Login } from "../pages/Login";
import { NotFound } from "../pages/NotFound";
import { Overview } from "../pages/Overview";
import { Remediation } from "../pages/Remediation";
import { Report } from "../pages/Report";
import { IncidentFeedProvider, SessionProvider } from "./context";

// 监控页带着图表库，按需加载，其他页面不下载它。
const Monitor = lazy(() => import("../pages/Monitor").then((module) => ({ default: module.Monitor })));

export function App() {
  // undefined: checking the session; null: signed out.
  const [session, setSession] = useState<Session | null | undefined>(undefined);
  const [model, setModel] = useState<ModelState | null>(null);

  useEffect(() => {
    let active = true;
    void getSession().then((value) => {
      if (active) setSession(value);
    }).catch(() => {
      if (active) setSession(null);
    });
    const signedOut = () => setSession(null);
    window.addEventListener(unauthorizedEvent, signedOut);
    return () => {
      active = false;
      window.removeEventListener(unauthorizedEvent, signedOut);
    };
  }, []);

  useEffect(() => {
    if (!session) return;
    let active = true;
    void getCurrentModel().then((state) => {
      if (active) setModel(state);
    }).catch(() => {
      // The console remains usable when LLM/model management is unavailable.
    });
    return () => {
      active = false;
    };
  }, [session]);

  const value = useMemo(() => session ? {
    session,
    model,
    setModel,
    signOut: () => {
      void logout().finally(() => setSession(null));
    },
  } : null, [session, model]);

  if (session === undefined) {
    return (
      <div className="grid min-h-dvh place-items-center" aria-busy="true" aria-label="正在检查登录状态">
        <BrandMark className="size-7 animate-pulse" />
      </div>
    );
  }
  if (session === null || !value) return <Login onLogin={setSession} />;

  return (
    <SessionProvider value={value}>
      <IncidentFeedProvider>
        <Routes>
          <Route element={<AppLayout />}>
            <Route index element={<Overview />} />
            <Route path="incidents" element={<Incidents />} />
            <Route path="incidents/:id" element={<IncidentDetailRoute />} />
            <Route path="monitor" element={<Suspense fallback={null}><Monitor /></Suspense>} />
            <Route path="remediation" element={<Remediation />} />
            <Route path="report" element={<Report />} />
            <Route path="changes" element={<Changes />} />
            <Route path="*" element={<NotFound />} />
          </Route>
        </Routes>
      </IncidentFeedProvider>
    </SessionProvider>
  );
}
