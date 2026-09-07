import "./style.css";
import { APIError, list, request } from "./api";
import { state, Host, t, applyPreferences, savePreferences } from "./state";
import {
  button,
  dialog,
  el,
  field,
  notify,
  panel,
  row,
  select,
  stack,
  refreshLocalizedControls,
} from "./ui";
import { enginePage, composePage } from "./engine";
import { hostsPage, filesPage, tunnelsPage } from "./connections";
import { commandsPage, schedulesPage, runsPage } from "./jobs";
import { terminalView, TerminalView } from "./terminal";
const app = document.querySelector<HTMLElement>("#app")!;
const notifications = el("div", "", "notifications");
notifications.id = "notifications";
notifications.setAttribute("aria-live", "polite");
document.body.append(notifications);
const pages = [
  "Overview",
  "Hosts",
  "Containers",
  "Compose",
  "Images",
  "Volumes",
  "Networks",
  "Terminal",
  "Files",
  "Tunnels",
  "Commands",
  "Schedules",
  "Jobs",
  "Settings",
];
const hostPages = new Set([
  "Containers",
  "Compose",
  "Images",
  "Volumes",
  "Networks",
  "Terminal",
  "Files",
  "Tunnels",
]);
type Tab = {
  id: number;
  page: string;
  hostId: string;
  node: HTMLElement;
  terminal?: TerminalView;
};
let tabs: Tab[] = [],
  activeId = 0,
  nextId = 1,
  version = { version: "Unavailable", updatedAt: "" },
  shellNode: HTMLElement,
  nav: HTMLElement,
  content: HTMLElement,
  tabstrip: HTMLElement;
function versionLine(): HTMLElement {
  let time = "Updated: unavailable";
  const date = new Date(version.updatedAt);
  if (version.updatedAt && Number.isFinite(date.valueOf()))
    time =
      "Updated: " +
      date.toLocaleString(undefined, {
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        second: "2-digit",
        timeZoneName: "short",
      });
  return el(
    "div",
    `Version ${version.version || "unavailable"} · ${time}`,
    "version",
  );
}
function appMark():HTMLImageElement{const mark=el('img');mark.src='/logo.svg';mark.alt='Container SSH Manager';mark.width=40;mark.height=40;mark.className='app-mark';return mark;}
async function reloadHosts(): Promise<void> {
  state.hosts = list<Host>(await request("/api/v1/hosts"));
  if (!state.hosts.some((h) => h.id === state.hostId))
    state.hostId =
      state.hosts.find((h) => h.id === "local")?.id || state.hosts[0]?.id || "";
  if (nav) renderNav();
}
function login(): void {
  tabs.forEach((tab) => tab.terminal?.dispose());
  tabs = [];
  applyPreferences();
  const password = field("Owner password", "", "password"),
    error = el("p", "", "form-error");
  password.autocomplete = "current-password";
  let signingIn = false;
  const submit = async () => {
    if (signingIn) return;
    signingIn = true;
    try {
      const value = password.value;
      password.value = "";
      await request("/api/v1/login", "POST", { password: value });
      await reloadHosts();
      shell();
      await openPage("Overview");
    } catch (e) {
      error.textContent = e instanceof Error ? e.message : String(e);
      password.focus();
    } finally {
      signingIn = false;
    }
  };
  password.addEventListener("keydown", (event: KeyboardEvent) => {
    if (event.key === "Enter") {
      event.preventDefault();
      void submit();
    }
  });
  const card = panel(
    "Container SSH Manager",
    versionLine(),
    el("h1", "Sign in"),
    el(
      "p",
      "Manage containers and SSH hosts from one authenticated workspace.",
    ),
    password,
    error,
    button("Sign in", submit, true),
  );
  card.classList.add("login");
  card.prepend(appMark());
  app.replaceChildren(card);
  setTimeout(() => password.focus(), 0);
}
function shell(): void {
  shellNode = el("div", "", "shell");
  nav = el("aside", "", "navigation");
  nav.setAttribute("aria-label", "Hosts and navigation");
  content = el("main", "", "main");
  tabstrip = el("div", "", "tabstrip");
  tabstrip.setAttribute("role", "tablist");
  const menu = button("Menu", () => {
    shellNode.classList.toggle("nav-open");
  });
  menu.id = "menu-toggle";
  const header = el("header", "", "topbar");
  header.append(
    row(menu, appMark(), el("strong", "Container SSH Manager")),
    versionLine(),
    row(
      button("Jobs", async () => dialog("Jobs", await runsPage())),
      button("Settings", () => openPage("Settings")),
      button("Command palette", () => commandPalette()),
      button("Sign out", async () => {
        await request("/api/v1/logout", "POST");
        login();
      }),
    ),
  );
  shellNode.append(header, nav, stack(tabstrip, content));
  shellNode.lastElementChild!.classList.add("workspace");
  app.replaceChildren(shellNode);
  renderNav();
  renderTabs();
}
function renderNav(): void {
  nav.replaceChildren();
  const hostSelect = select(
    t("Select a host"),
    state.hosts.map((h) => ({ value: h.id, label: h.name })),
    state.hostId,
  );
  hostSelect.addEventListener("change", () => {
    state.hostId = hostSelect.value;
    const page = tabs.find((tab) => tab.id === activeId)?.page || "Overview";
    void openPage(page).catch((e) => notify(e.message, true));
  });
  const search = field("Search"),
    links = stack();
  const draw = () => {
    links.replaceChildren(
      ...pages
        .filter((page) =>
          t(page).toLowerCase().includes(search.value.toLowerCase()),
        )
        .map((page) => button(page, () => openPage(page))),
    );
  };
  search.addEventListener("input", draw);
  draw();
  nav.append(
    el("div", "WORKSPACE", "eyebrow"),
    hostSelect,
    button("Add host", () => openPage("Hosts")),
    search,
    links,
  );
}
function commandPalette(): void {
  const search = field("Search"),
    results = stack();
  let popup: any;
  const draw = () => {
    results.replaceChildren(
      ...pages
        .filter((page) =>
          t(page).toLowerCase().includes(search.value.toLowerCase()),
        )
        .map((page) =>
          button(t(page), async () => {
            popup.close();
            await openPage(page);
          }),
        ),
    );
  };
  search.addEventListener("input", draw);
  draw();
  popup = dialog("Command palette", stack(search, results));
  setTimeout(() => search.focus(), 0);
}
function renderTabs(): void {
  if (!tabstrip) return;
  tabstrip.replaceChildren();
  tabs.forEach((tab) => {
    const host = state.hosts.find((h) => h.id === tab.hostId),
      title =
        t(tab.page) +
        (hostPages.has(tab.page) && host ? " · " + host.name : "");
    const activate = button(title, () => activateTab(tab));
    activate.setAttribute("role", "tab");
    activate.setAttribute("aria-selected", String(tab.id === activeId));
    activate.setAttribute("aria-controls", "page-" + tab.id);
    tabstrip.append(
      row(
        activate,
        button("×", () => {
          const closeTab = () => {
            tab.terminal?.dispose();
            tab.node.remove();
            tabs = tabs.filter((t) => t.id !== tab.id);
            if (activeId === tab.id) {
              if (tabs.length) activateTab(tabs.at(-1)!);
              else void openPage("Overview");
            }
            renderTabs();
          };
          if (tab.terminal || tab.node.querySelector(".wide"))
            dialog(
              "Close workspace tab",
              el(
                "p",
                tab.terminal
                  ? "Closing ends this terminal session and its active process."
                  : "Unsaved entries in this tab will be lost.",
              ),
              closeTab,
              "Close tab",
            );
          else closeTab();
        }),
      ),
    );
    tabstrip.lastElementChild!.lastElementChild!.setAttribute(
      "aria-label",
      "Close " + title,
    );
  });
}
function activateTab(tab: Tab): void {
  activeId = tab.id;
  state.hostId = tab.hostId || state.hostId;
  tabs.forEach((item) => (item.node.hidden = item.id !== tab.id));
  renderTabs();
  renderNav();
  tab.terminal?.resize();
  shellNode.classList.remove("nav-open");
}
async function openPage(page: string): Promise<void> {
  if (hostPages.has(page) && !state.hostId) {
    notify("Add or select a stored host first", true);
    page = "Hosts";
  }
  const hostId = hostPages.has(page) ? state.hostId : "";
  const existing = tabs.find(
    (tab) => tab.page === page && tab.hostId === hostId,
  );
  if (existing) {
    activateTab(existing);
    return;
  }
  const tab: Tab = {
    id: nextId++,
    page,
    hostId,
    node: el("section", "", "page"),
  };
  tab.node.id = "page-" + tab.id;
  tab.node.setAttribute("role", "tabpanel");
  tab.node.append(el("h1", t(page)), el("p", "Loading live data…"));
  tabs.push(tab);
  content.append(tab.node);
  activateTab(tab);
  const load = async () => {
    try {
      let view: HTMLElement;
      if(hostId==='local'&&['Files','Tunnels'].includes(page))view=panel('SSH connection required',el('p','The local host is engine-only. Select a stored SSH host to use file transfer or tunnels.'),button('Hosts',()=>openPage('Hosts')));
      else if (["Containers", "Images", "Volumes", "Networks"].includes(page))
        view = await enginePage(page.toLowerCase(), hostId);
      else if (page === "Compose") view = await composePage(hostId);
      else if (page === "Hosts") view = await hostsPage(reloadHosts);
      else if (page === "Files") view = await filesPage(hostId);
      else if (page === "Tunnels") view = await tunnelsPage(hostId);
      else if (page === "Commands") view = await commandsPage();
      else if (page === "Schedules") view = await schedulesPage();
      else if (page === "Jobs") view = await runsPage();
      else if (page === "Settings") view = settings();
      else if (page === "Terminal") {
        if (hostId === "local") {
          view = panel(
            "SSH connection required",
            el(
              "p",
              "The local host provides the container engine. Select a stored SSH host for an interactive terminal.",
            ),
          );
        } else {
          tab.terminal = terminalView(hostId);
          view = tab.terminal.node;
        }
      } else view = overview();
      if (!tabs.includes(tab)) return;
      tab.node.replaceChildren(
        row(
          el("h1", t(page)),
          button("Reload view", async () => {
            tab.terminal?.dispose();
            await load();
          }),
        ),
        view,
      );
      if (hostId) {
        const host = state.hosts.find((h) => h.id === hostId);
        tab.node.insertBefore(
          el(
            "p",
            host
              ? `${host.name} · ${host.id === "local" ? "Local engine" : `${host.user}@${host.address}:${host.port}`}`
              : hostId,
            "host-target",
          ),
          view,
        );
      }
      if (activeId === tab.id) tab.terminal?.resize();
    } catch (error) {
      if (error instanceof APIError && error.status === 401) {
        login();
        return;
      }
      if (tabs.includes(tab))
        tab.node.replaceChildren(
          el("h1", t(page)),
          panel(
            "Unable to load",
            el("p", (error as Error).message),
            button("Retry", load),
          ),
        );
    }
  };
  await load();
}
function overview(): HTMLElement {
  return stack(
    panel(
      "Your infrastructure",
      el(
        "p",
        `${state.hosts.length} stored hosts. Choose a host and open a workspace tab to inspect its live resources.`,
      ),
      row(
        button("Hosts", () => openPage("Hosts"), true),
        button("Containers", () => openPage("Containers")),
        button("Commands", () => openPage("Commands")),
      ),
    ),
    panel(
      "Execution and security",
      el(
        "p",
        "Commands run only on selected stored hosts. Schedules pin immutable revisions and disclose AUTOAPPROVED execution. Credential values are write-only; host keys require explicit enrollment.",
      ),
    ),
    panel(
      "Workspace",
      el(
        "p",
        "Open tabs retain their host context. Terminal reconnects attach to the same in-memory server session. Use the Jobs drawer to inspect outcomes and request cancellation.",
      ),
    ),
  );
}
function settings(): HTMLElement {
  const p = state.preferences,
    language = select(
      "Language",
      [
        { value: "en", label: "English" },
        { value: "zh", label: "廣東話" },
        { value: "both", label: "English + 廣東話" },
      ],
      p.language,
    ),
    theme = select(
      "Appearance",
      [
        { value: "dark", label: "Dark" },
        { value: "light", label: "Light" },
        { value: "system", label: "Follow device" },
      ],
      p.theme,
    ),
    density = select(
      "Density",
      [
        { value: "comfortable", label: "Comfortable" },
        { value: "compact", label: "Compact" },
      ],
      p.density,
    ),
    font = field("Text size (14 to 24 px)", String(p.fontSize), "number");
  return stack(
    panel(
      "Local preferences",
      language,
      theme,
      density,
      font,
      button(
        "Save preferences",
        () => {
          const fontSize = Number(font.value);
          if (!Number.isFinite(fontSize) || fontSize < 14 || fontSize > 24)
            throw Error("Text size must be 14 to 24 px");
          const stored = savePreferences({
            language: language.value,
            theme: theme.value,
            density: density.value,
            fontSize,
          });
          renderNav();
          renderTabs();
          tabs.forEach((tab) => {
            const heading = tab.node.querySelector("h1");
            if (heading) heading.textContent = t(tab.page);
          });
          refreshLocalizedControls();
          notify(
            stored
              ? "Preferences saved on this device"
              : "Preferences applied for this session; browser storage is unavailable.",
          );
        },
        true,
      ),
    ),
    panel(
      "Build provenance",
      versionLine(),
      el(
        "p",
        "The version and update time come from the running server build. Missing provenance is reported as unavailable.",
      ),
    ),
  );
}
async function init(): Promise<void> {
  applyPreferences();
  app.replaceChildren(el("p", "Loading Container SSH Manager…"));
  try {
    version = await request("/api/v1/version");
  } catch {
    version = { version: "Unavailable", updatedAt: "" };
  }
  try {
    const session = await request("/api/v1/session");
    if (!session.authenticated) {
      login();
      return;
    }
    await reloadHosts();
    shell();
    await openPage("Overview");
  } catch (error) {
    if (error instanceof APIError && error.status === 401) {
      login();
      return;
    }
    app.replaceChildren(
      panel(
        "Unable to connect",
        versionLine(),
        el("p", (error as Error).message),
        button("Retry", init),
      ),
    );
  }
}
void init();
document.addEventListener("keydown", (event: KeyboardEvent) => {
  if (
    (event.ctrlKey || event.metaKey) &&
    event.shiftKey &&
    event.key.toLowerCase() === "p" &&
    app.contains(shellNode)
  ) {
    event.preventDefault();
    commandPalette();
  }
});
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
  if (state.preferences.theme === "system") applyPreferences();
});
