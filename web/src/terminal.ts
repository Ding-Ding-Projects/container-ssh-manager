import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { hostURL, url } from "./api";
import { button, dialog, el, row, stack } from "./ui";
export type TerminalView = {
  node: HTMLElement;
  dispose: () => void;
  resize: () => void;
};
export function terminalView(hostId: string): TerminalView {
  const node = stack(),
    status = el("span", "Disconnected"),
    surface = el("div", "", "terminal-surface"),
    terminal = new Terminal({
      cursorBlink: true,
      scrollback: 3000,
      fontFamily: "Consolas, monospace",
      fontSize: 14,
      theme: { background: "#101419", foreground: "#e2e8ef" },
    }),
    fit = new FitAddon();
  terminal.loadAddon(fit);
  let ws: WebSocket | undefined,
    sessionId = "",
    disposed = false,
    retryTimer: ReturnType<typeof setTimeout> | undefined,
    attempt = 0,
    ended = false;
  const send = (frame: unknown) => {
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify(frame));
  };
  const resize = () => {
    if (surface.clientWidth && surface.clientHeight) {
      fit.fit();
      send({ type: "resize", cols: terminal.cols, rows: terminal.rows });
    }
  };
  const connect = () => {
    if (disposed) return;
    if (ws) {
      ws.onclose = null;
      ws.close();
    }
    clearTimeout(retryTimer);
    status.textContent = sessionId ? "Reattaching saved session" : "Connecting";
    const endpoint = new URL(
      url(hostURL(hostId, "/terminal"), { session: sessionId || undefined }),
      location.href,
    );
    endpoint.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(endpoint);
    ws.onopen = () => {
      attempt = 0;
      terminal.reset();
      resize();
    };
    ws.onmessage = (event) => {
      try {
        const frame = JSON.parse(event.data);
        if (frame.type === "output" && typeof frame.data === "string")
          terminal.write(frame.data);
        if (frame.type === "status") {
          if (frame.sessionId) sessionId = frame.sessionId;
          status.textContent =
            frame.state +
            (frame.exitCode === undefined ? "" : ` · exit ${frame.exitCode}`);
          ended = ["closed", "exited", "failed"].includes(frame.state);
          if (frame.state === "connected") resize();
        }
      } catch {
        status.textContent = "Invalid terminal frame";
        ws?.close();
      }
    };
    ws.onerror = () => {
      status.textContent = "Connection unavailable";
    };
    ws.onclose = () => {
      if (disposed || ended) return;
      status.textContent = "Disconnected. Session retained briefly on server.";
      if (sessionId && attempt < 3) {
        attempt++;
        retryTimer = setTimeout(connect, attempt * 2000);
      } else status.textContent += " Use Reattach to retry.";
    };
  };
  node.append(
    row(
      status,
      button("Reattach", () => {
        ended = false;
        attempt = 0;
        connect();
      }),
      button("New session", () =>
        dialog(
          "Replace terminal session",
          el("p", "End the current session and open a new shell?"),
          () => {
            send({ type: "close" });
            sessionId = "";
            ended = false;
            connect();
          },
          "New session",
        ),
      ),
    ),
    surface,
  );
  requestAnimationFrame(() => {
    if (disposed) return;
    terminal.open(surface);
    resize();
    connect();
  });
  terminal.onData((data) => send({ type: "input", data }));
  const observer = new ResizeObserver(resize);
  observer.observe(surface);
  return {
    node,
    resize,
    dispose: () => {
      disposed = true;
      clearTimeout(retryTimer);
      send({ type: "close" });
      ws?.close();
      observer.disconnect();
      terminal.dispose();
    },
  };
}
