import { cantonese } from "./locales";
export type Host = {
  id: string;
  name: string;
  address: string;
  port: number;
  user: string;
  credentialId: string;
  group?: string;
  tags?: string[];
  jumpIds?: string[];
  hostKey?: string;
};
export type Preferences = {
  language: "en" | "zh" | "both";
  theme: "dark" | "light" | "system";
  density: "comfortable" | "compact";
  fontSize: number;
};
export const defaults: Preferences = {
  language: "en",
  theme: "dark",
  density: "comfortable",
  fontSize: 16,
};
export function readPreferences(
  storage?: Pick<Storage, "getItem">,
): Preferences {
  try {
    const p = JSON.parse(
      (storage || localStorage).getItem("manager.preferences.v1") || "{}",
    );
    return {
      language: ["en", "zh", "both"].includes(p?.language)
        ? p.language
        : defaults.language,
      theme: ["dark", "light", "system"].includes(p?.theme)
        ? p.theme
        : defaults.theme,
      density: p?.density === "compact" ? "compact" : "comfortable",
      fontSize:
        Number.isFinite(p?.fontSize) && p.fontSize >= 14 && p.fontSize <= 24
          ? p.fontSize
          : 16,
    };
  } catch {
    return { ...defaults };
  }
}
export const state = {
  hosts: [] as Host[],
  hostId: "",
  preferences: readPreferences(),
};
export function savePreferences(p: Preferences): boolean {
  state.preferences = p;
  applyPreferences();
  try {
    localStorage.setItem("manager.preferences.v1", JSON.stringify(p));
    return true;
  } catch {
    return false;
  }
}
export function applyPreferences(): void {
  const p = state.preferences;
  document.documentElement.dataset.theme =
    p.theme === "system"
      ? matchMedia("(prefers-color-scheme: dark)").matches
        ? "dark"
        : "light"
      : p.theme;
  document.documentElement.dataset.density = p.density;
  document.documentElement.lang = p.language === "zh" ? "zh-HK" : "en";
  document.documentElement.style.fontSize = p.fontSize + "px";
}
const words: Record<string, string> = {
  Overview: "總覽",
  Hosts: "主機",
  Containers: "容器",
  Images: "映像",
  Volumes: "儲存卷",
  Networks: "網絡",
  Compose: "Compose 專案",
  Files: "檔案",
  Tunnels: "通道",
  Commands: "指令",
  Schedules: "排程",
  Settings: "設定",
  Terminal: "終端機",
  Jobs: "工作",
  Refresh: "重新整理",
  "Sign in": "登入",
  "Sign out": "登出",
  "Add host": "新增主機",
  Credentials: "憑證",
  "Select a host": "選擇主機",
  Search: "搜尋",
  Menu: "選單",
};
export function t(text: string): string {
  const zh = cantonese[text] || words[text];
  return !zh || state.preferences.language === "en"
    ? text
    : state.preferences.language === "zh"
      ? zh
      : `${text} · ${zh}`;
}
export function selectedHost(): Host {
  const h = state.hosts.find((h) => h.id === state.hostId);
  if (!h) throw Error("Select a stored host first");
  return h;
}
