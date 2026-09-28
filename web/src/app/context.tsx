import { ReactNode, createContext, useCallback, useContext, useEffect, useMemo, useRef, useState } from "react";

import { Incident, ModelState, Session, listIncidents } from "../api";

interface SessionValue {
  session: Session;
  model: ModelState | null;
  setModel: (state: ModelState) => void;
  signOut: () => void;
}

const SessionContext = createContext<SessionValue | null>(null);

export function SessionProvider({ value, children }: { value: SessionValue; children: ReactNode }) {
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): SessionValue {
  const value = useContext(SessionContext);
  if (!value) throw new Error("useSession outside SessionProvider");
  return value;
}

interface FeedValue {
  incidents: Incident[];
  loaded: boolean;
  firing: number;
  refresh: () => void;
}

const FeedContext = createContext<FeedValue>({ incidents: [], loaded: false, firing: 0, refresh: () => undefined });

const feedIntervalMs = 30_000;

// IncidentFeedProvider keeps the latest incidents for the sidebar, the command
// palette and the overview. It is a convenience view: pages that need a filter
// query the server themselves, and a failed poll keeps the previous list.
export function IncidentFeedProvider({ children }: { children: ReactNode }) {
  const [incidents, setIncidents] = useState<Incident[]>([]);
  const [loaded, setLoaded] = useState(false);
  const inFlight = useRef(false);

  const refresh = useCallback(() => {
    if (inFlight.current) return;
    inFlight.current = true;
    void listIncidents("", 50)
      .then((rows) => {
        setIncidents(rows);
        setLoaded(true);
      })
      .catch(() => setLoaded(true))
      .finally(() => {
        inFlight.current = false;
      });
  }, []);

  useEffect(() => {
    refresh();
    const timer = window.setInterval(refresh, feedIntervalMs);
    return () => window.clearInterval(timer);
  }, [refresh]);

  const value = useMemo(() => ({
    incidents,
    loaded,
    firing: incidents.filter((incident) => incident.status === "firing").length,
    refresh,
  }), [incidents, loaded, refresh]);
  return <FeedContext.Provider value={value}>{children}</FeedContext.Provider>;
}

export function useIncidentFeed(): FeedValue {
  return useContext(FeedContext);
}
