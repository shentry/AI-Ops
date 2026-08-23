import { useEffect, useState } from "react";
import { createRoot } from "react-dom/client";

import { getCurrentModel, ModelState } from "./api";
import { IncidentPicker } from "./pages/IncidentPicker";
import { IncidentRoom } from "./pages/IncidentRoom";
import "./styles.css";

const anonymousActorName = "匿名用户";

function App() {
  const [modelState, setModelState] = useState<ModelState | null>(null);
  const incidentID = incidentIDFromPath(window.location.pathname);

  useEffect(() => {
    let active = true;
    void getCurrentModel().then((state) => {
      if (active) setModelState(state);
    }).catch(() => {
      // The control room remains usable when LLM/model management is unavailable.
    });
    return () => {
      active = false;
    };
  }, []);

  if (incidentID === null) {
    return <IncidentPicker actorName={anonymousActorName} modelState={modelState} onModelChanged={setModelState} />;
  }
  return <IncidentRoom incidentId={incidentID} actorName={anonymousActorName} modelState={modelState} onModelChanged={setModelState} />;
}

function incidentIDFromPath(pathname: string): number | null {
  const match = pathname.match(/^\/incidents\/(\d+)(?:\/|$)/);
  if (!match) return null;
  const value = Number(match[1]);
  return Number.isSafeInteger(value) && value > 0 ? value : null;
}

const root = document.getElementById("root");
if (!root) throw new Error("Missing #root element");
createRoot(root).render(<App />);
