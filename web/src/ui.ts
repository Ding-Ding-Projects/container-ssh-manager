import "@material/web/all.js";
import { t } from "./state";

export type Action = () => void | Promise<void>;
export function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  text = "",
  cls = "",
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  node.textContent = text;
  node.className = cls;
  return node;
}
export function material(tag: string, text = ""): any {
  const node = document.createElement(tag);
  node.textContent = text;
  return node;
}
export function stack(...children: (Node | string | undefined)[]): HTMLElement {
  const node = el("div", "", "stack");
  children.forEach((child) => {
    if (child !== undefined) node.append(child);
  });
  return node;
}
export function row(...children: (Node | string | undefined)[]): HTMLElement {
  const node = stack(...children);
  node.className = "row";
  return node;
}
export function button(
  text: string,
  action: Action,
  primary = false,
): HTMLElement {
  const node = material(
    primary ? "md-filled-button" : "md-outlined-button",
    t(text),
  );
  node.dataset.copy = text;
  node.dataset.action = text.toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'');
  node.addEventListener("click", async () => {
    if (node.disabled) return;
    node.disabled = true;
    try {
      await action();
    } catch (error) {
      notify(error instanceof Error ? error.message : String(error), true);
    } finally {
      node.disabled = false;
    }
  });
  return node;
}
export function field(label: string, value = "", type = "text"): any {
  const node = material("md-outlined-text-field");
  node.label = t(label);
  node.dataset.copy = label;
  node.dataset.field = label.toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'');
  node.dataset.copyProperty = "label";
  node.value = value;
  node.type = type;
  if (type === "textarea") {
    node.rows = 5;
    node.classList.add("wide");
  }
  node.setAttribute("aria-label", t(label));
  return node;
}
export function select(
  label: string,
  choices: { value: string; label: string }[],
  value = "",
): any {
  const node = material("md-outlined-select");
  node.dataset.field = label.toLowerCase().replace(/[^a-z0-9]+/g,'-').replace(/^-|-$/g,'');
  node.label = t(label);
  node.dataset.copy = label;
  node.dataset.copyProperty = "label";
  node.setAttribute("aria-label", t(label));
  choices.forEach((choice) => {
    const option = material("md-select-option");
    option.value = choice.value;
    option.append(el("div", choice.label));
    option.firstElementChild!.setAttribute("slot", "headline");
    node.append(option);
  });
  node.value = value;
  return node;
}
export function check(
  label: string,
  checked = false,
): { node: HTMLElement; input: any } {
  const input = material("md-checkbox");
  input.checked = checked;
  input.setAttribute("aria-label", t(label));
  const text = el("span", t(label));
  text.dataset.copy = label;
  const node = el("label", "", "check");
  node.append(input, text);
  return { node, input };
}
export function panel(title: string, ...children: Node[]): HTMLElement {
  const node = el("section", "", "panel");
  node.append(el("h2", title), ...children);
  return node;
}
export function pre(data: unknown): HTMLElement {
  return el(
    "pre",
    typeof data === "string" ? data : JSON.stringify(data, null, 2),
    "output",
  );
}
const notificationState = new WeakMap<HTMLElement,{message:string;error:boolean;timer?:ReturnType<typeof setTimeout>}>();
export function notify(message: string, error = false): void {
  const region = document.getElementById("notifications");
  if (!region) return;
  const remove=(node:HTMLElement)=>{clearTimeout(notificationState.get(node)?.timer);node.remove();};
  const expire=(node:HTMLElement)=>{const data=notificationState.get(node);if(!data||data.error)return;clearTimeout(data.timer);data.timer=setTimeout(()=>remove(node),8000);};
  const existing=Array.from(region.children).find(node=>{const data=notificationState.get(node as HTMLElement);return data?.message===message&&data.error===error;}) as HTMLElement|undefined;
  if(existing){region.append(existing);expire(existing);return;}
  while(region.childElementCount>=4){const firstInfo=Array.from(region.children).find(node=>!notificationState.get(node as HTMLElement)?.error) as HTMLElement|undefined;if(!firstInfo&&!error)return;remove(firstInfo||region.firstElementChild as HTMLElement);}
  const node = el("div", "", error ? "notice error" : "notice");
  notificationState.set(node,{message,error});
  node.setAttribute("role", error ? "alert" : "status");
  node.append(
    el("span", message),
    button("Dismiss", () => remove(node)),
  );
  node.addEventListener('pointerenter',()=>clearTimeout(notificationState.get(node)?.timer));
  node.addEventListener('pointerleave',()=>expire(node));
  node.addEventListener('focusin',()=>clearTimeout(notificationState.get(node)?.timer));
  node.addEventListener('focusout',()=>expire(node));
  region.append(node);
  expire(node);
}
export function dialog(
  title: string,
  content: Node,
  save?: Action,
  saveLabel = "Save",
): any {
  const d = material("md-dialog");
  const headline = el("div", t(title));
  headline.dataset.copy = title;
  headline.slot = "headline";
  const body = el("div", "", "dialog-body");
  body.slot = "content";
  body.append(content);
  const actions = row();
  actions.slot = "actions";
  actions.append(button("Close", () => d.close()));
  if (save)
    actions.append(
      button(
        saveLabel,
        async () => {
          await save();
          d.close();
        },
        true,
      ),
    );
  d.append(headline, body, actions);
  document.body.append(d);
  d.addEventListener("closed", () => d.remove(), { once: true });
  d.show();
  return d;
}
export function refreshLocalizedControls(): void {
  document.querySelectorAll<HTMLElement>("[data-copy]").forEach((node: any) => {
    const translated = t(node.dataset.copy);
    if (node.dataset.copyProperty === "label") {
      node.label = translated;
      node.setAttribute("aria-label", translated);
    } else node.textContent = translated;
    if (node.parentElement?.classList.contains("check"))
      node.parentElement
        .querySelector("md-checkbox")
        ?.setAttribute("aria-label", translated);
  });
}
export function confirmAction(
  title: string,
  target: string,
  action: Action,
): void {
  const proof = field("Type REMOVE to confirm");
  dialog(
    title,
    stack(
      el(
        "p",
        `Target: ${target}. This changes live resources and cannot be undone here.`,
      ),
      proof,
    ),
    async () => {
      if (proof.value !== "REMOVE") throw Error("Confirmation must be REMOVE");
      await action();
    },
    "Remove",
  );
}
export const values = (text: string) =>
  text
    .split("\n")
    .map((v) => v.trim())
    .filter(Boolean);
export function pairs(text: string): Record<string, string> {
  return Object.fromEntries(
    values(text).map((line) => {
      const split = line.indexOf("=");
      if (split < 1) throw Error("Each entry must use KEY=VALUE");
      return [line.slice(0, split), line.slice(split + 1)];
    }),
  );
}
export function positiveInteger(
  value: string,
  name: string,
  max = 65535,
): number {
  const n = Number(value);
  if (!Number.isInteger(n) || n < 1 || n > max)
    throw Error(`${name} must be an integer from 1 to ${max}`);
  return n;
}
export function objectJSON(
  value: string,
  label: string,
): Record<string, unknown> {
  const out = JSON.parse(value || "{}");
  if (!out || typeof out !== "object" || Array.isArray(out))
    throw Error(`${label} must be a JSON object`);
  return out;
}
