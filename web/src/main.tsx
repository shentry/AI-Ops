import "@fontsource/ibm-plex-sans/400.css";
import "@fontsource/ibm-plex-sans/500.css";
import "@fontsource/ibm-plex-sans/600.css";
import "@fontsource/ibm-plex-mono/400.css";
import "@fontsource/ibm-plex-mono/500.css";
import "./index.css";

import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router";

import { App } from "./app/App";
import { applyTheme, readTheme } from "./app/theme";

// CSP 禁止内联脚本，主题只能在这里、首帧渲染前应用；index.html 默认已是深色。
applyTheme(readTheme());

const root = document.getElementById("root");
if (!root) throw new Error("Missing #root element");
createRoot(root).render(
  <BrowserRouter>
    <App />
  </BrowserRouter>,
);
