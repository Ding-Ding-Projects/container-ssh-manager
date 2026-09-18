import { beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
vi.mock("@material/web/all.js", () => ({}));
import {
  button,
  dialog,
  field,
  el,
  pairs,
  positiveInteger,
  objectJSON,
  refreshLocalizedControls,
  notify,
} from "./ui";
import { APIError, request, url, engineURL, list } from "./api";
import { readPreferences, state } from "./state";
import { enginePage,composePage } from "./engine";
import { filesPage, hostsPage } from "./connections";
import { commandsPage, schedulesPage,hostChoices } from "./jobs";
class TestDialog extends HTMLElement {
  open = false;
  show() {
    this.open = true;
  }
  close() {
    this.open = false;
    this.dispatchEvent(new Event("closed"));
  }
}
beforeAll(() => {
  if (!customElements.get("md-dialog"))
    customElements.define("md-dialog", TestDialog);
});
beforeEach(() => {
  document.body.innerHTML = '<div id="notifications"></div>';
  vi.restoreAllMocks();
  state.preferences.language = "en";
  state.hosts = [
    {
      id: "host & one",
      name: "Primary",
      address: "host.test",
      user: "owner",
      port: 22,
      credentialId: "credential",
    },
  ];
  state.hostId = "host & one";
});
const response = (body: unknown, status = 200, type = "application/json") =>
  new Response(type.includes("json") ? JSON.stringify(body) : String(body), {
    status,
    headers: { "content-type": type },
  });
const click = async (label: string, scope: ParentNode = document) => {
  const target = Array.from(
    scope.querySelectorAll("md-filled-button,md-outlined-button"),
  ).find((node) => node.textContent === label) as HTMLElement;
  expect(target, `button ${label}`).toBeTruthy();
  target.click();
  await new Promise((resolve) => setTimeout(resolve, 0));
};
describe("safe components and dialog actions", () => {
  it('coalesces repeated information and keeps a bounded queue',()=>{for(let count=0;count<8;count++)notify('Preferences saved');expect(document.querySelectorAll('.notice')).toHaveLength(1);for(let count=0;count<8;count++)notify('Information '+count);expect(document.querySelectorAll('.notice')).toHaveLength(4);});
  it('expires information but keeps errors until dismissed',()=>{vi.useFakeTimers();try{notify('Saved');notify('Unable to save',true);vi.advanceTimersByTime(8000);expect(document.querySelectorAll('.notice')).toHaveLength(1);expect(document.querySelector('[role=alert]')?.textContent).toContain('Unable to save');vi.advanceTimersByTime(60000);expect(document.querySelector('[role=alert]')).not.toBeNull();}finally{vi.useRealTimers();}});
  it('does not evict persistent errors to show routine information',()=>{for(let count=0;count<4;count++)notify('Error '+count,true);notify('Preferences saved');expect(document.querySelectorAll('[role=alert]')).toHaveLength(4);expect(document.querySelector('[role=status]')).toBeNull();});
  it("updates existing labels in Cantonese and bilingual modes and restores English", () => {
    const action = button("Save", () => {}),
      input = field("Name");
    document.body.append(action, input);
    state.preferences.language = "zh";
    refreshLocalizedControls();
    expect(action.textContent).toBe("儲存");
    expect(input.label).toBe("名稱");
    state.preferences.language = "both";
    refreshLocalizedControls();
    expect(action.textContent).toBe("Save · 儲存");
    expect(input.getAttribute("aria-label")).toBe("Name · 名稱");
    state.preferences.language = "en";
    refreshLocalizedControls();
    expect(action.textContent).toBe("Save");
  });
  it("renders hostile names as text, never markup", () => {
    const node = el("p", "<img src=x onerror=alert(1)>");
    expect(node.querySelector("img")).toBeNull();
    expect(node.textContent).toContain("<img");
  });
  it("deduplicates pending button actions and restores state", async () => {
    let finish!: () => void;
    const action = vi.fn(
      () => new Promise<void>((resolve) => (finish = resolve)),
    );
    const node = button("Apply", action) as any;
    node.click();
    node.click();
    expect(action).toHaveBeenCalledTimes(1);
    expect(node.disabled).toBe(true);
    finish();
    await Promise.resolve();
    expect(node.disabled).toBe(false);
  });
  it("keeps failed dialog save open with an alert and preserves entered text", async () => {
    const content = field("Draft", "keep me"),
      d = dialog("Editor", content, async () => {
        throw Error("Conflict");
      });
    await click("Save", d);
    expect(d.open).toBe(true);
    expect(content.value).toBe("keep me");
    expect(document.querySelector("[role=alert]")?.textContent).toContain(
      "Conflict",
    );
  });
  it("closes and removes a successfully saved dialog", async () => {
    const save = vi.fn(),
      d = dialog("Editor", el("p", "content"), save);
    await click("Save", d);
    expect(save).toHaveBeenCalledOnce();
    expect(d.isConnected).toBe(false);
  });
  it("does not call save when closing a dialog", async () => {
    const save = vi.fn(),
      d = dialog("Editor", el("p"), save);
    await click("Close", d);
    expect(save).not.toHaveBeenCalled();
    expect(d.isConnected).toBe(false);
  });
  it("parses driver values without prototype pollution", () => {
    const result = pairs("__proto__=value\nKEY=a=b");
    expect(Object.getPrototypeOf(result)).toBe(Object.prototype);
    expect(result.KEY).toBe("a=b");
    expect(Object.hasOwn(result, "__proto__")).toBe(true);
  });
  it.each(["zero", "0", "-1", "65536", "1.2"])(
    "rejects invalid port %s",
    (value) => expect(() => positiveInteger(value, "Port")).toThrow(),
  );
  it("refuses arrays as advanced configuration", () =>
    expect(() => objectJSON("[]", "HostConfig")).toThrow());
});
describe("transport contracts", () => {
  it("encodes resource identifiers and query values separately", () => {
    const endpoint = url(engineURL("containers", "a/b ?"), {
      hostId: "host & one",
      force: false,
    });
    expect(endpoint).toBe(
      "/api/v1/engine/containers/a%2Fb%20%3F?hostId=host+%26+one&force=false",
    );
  });
  it("returns the actual HTTP error status and safe error text", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(response({ error: "Changed on server" }, 409)),
    );
    await expect(request("/file", "PUT", {})).rejects.toMatchObject({
      status: 409,
      message: "Changed on server",
    });
  });
  it("retains binary upload bodies without JSON encoding", async () => {
    const fetcher = vi.fn().mockResolvedValue(response({}));
    vi.stubGlobal("fetch", fetcher);
    const blob = new Blob(["content"]);
    await request("/upload", "POST", blob, true);
    expect(fetcher.mock.calls[0][1].body).toBe(blob);
    expect(fetcher.mock.calls[0][1].headers).toEqual({});
  });
  it("supports the volume response envelope and rejects other objects", () => {
    expect(list({ Volumes: [{ Name: "data" }] }, "Volumes")).toHaveLength(1);
    expect(list({ Volumes: null }, "Volumes")).toEqual([]);
    expect(() => list({ unexpected: [] })).toThrow();
  });
});
describe("preferences", () => {
  it.each(["null", "{", '{"theme":"wrong","language":"wrong","fontSize":200}'])(
    "recovers invalid stored preferences %s",
    (value) => {
      expect(readPreferences({ getItem: () => value })).toMatchObject({
        theme: "dark",
        language: "en",
        fontSize: 16,
      });
    },
  );
  it("recovers unavailable browser storage", () =>
    expect(
      readPreferences({
        getItem: () => {
          throw Error("disabled");
        },
      }),
    ).toMatchObject({ theme: "dark" }));
  it("loads all valid language/theme preferences", () =>
    expect(
      readPreferences({
        getItem: () =>
          '{"theme":"light","language":"both","density":"compact","fontSize":20}',
      }),
    ).toEqual({
      theme: "light",
      language: "both",
      density: "compact",
      fontSize: 20,
    }));
});
describe("management controls bind real API routes", () => {
  it('Compose down preserves images and volumes and deployment always runs detached',async()=>{
    const project={id:'project-one',hostId:'host & one',name:'Production',path:'/srv/project'},fetcher=vi.fn().mockImplementation((path:string,init:RequestInit)=>Promise.resolve(response(init.method==='POST'?{ok:true}:[project])));vi.stubGlobal('fetch',fetcher);document.body.append(await composePage());await click('Down');let form=document.querySelector('md-dialog')!;expect(form.textContent).toContain('Volumes and images are preserved');expect(form.querySelector('md-checkbox')).toBeNull();(form.querySelector('md-outlined-text-field') as any).value='REMOVE';await click('Remove',form);const down=fetcher.mock.calls.find(call=>call[0].endsWith('/down'));expect(JSON.parse(down?.[1].body)).toEqual({});await click('Close',document.querySelector('md-dialog')!);await click('Deploy');form=document.querySelector('md-dialog')!;expect(form.querySelectorAll('md-checkbox')).toHaveLength(2);await click('Deploy',form);const deploy=fetcher.mock.calls.find(call=>call[0].endsWith('/deploy'));expect(JSON.parse(deploy?.[1].body)).toEqual({pull:true,build:false,detach:true});
  });
  it('blocks Compose editing and lifecycle actions until confirmed file recovery succeeds',async()=>{
    const project={id:'project-one',hostId:'host & one',name:'Production',path:'/srv/project',pending:{state:'recovery_required'},revisions:[{number:1,createdAt:'2026-09-07'}]};let recovered=false;
    const fetcher=vi.fn().mockImplementation((path:string,init:RequestInit)=>{if(path.endsWith('/recover')){recovered=true;return Promise.resolve(response({ok:true}));}const record={...project,...(recovered?{pending:undefined}:{})};return Promise.resolve(response(path.endsWith('/projects')?[record]:record));});vi.stubGlobal('fetch',fetcher);document.body.append(await composePage());
    for(const action of ['read-and-edit-files','validate','deploy','stop','down']){const control=document.querySelector(`[data-action="${action}"]`) as any;expect(control.disabled).toBe(true);control.click();}expect(fetcher).toHaveBeenCalledTimes(1);
    await click('Revisions');const revisions=document.querySelector('md-dialog')!;expect((revisions.querySelector('[data-action="restore"]') as any).disabled).toBe(true);await click('Close',revisions);
    await click('Recover files');const recovery=document.querySelector('md-dialog')!;expect(recovery.textContent).toContain('Production');expect(recovery.textContent).toContain('/srv/project');expect(recovery.textContent).toContain('project-one');await click('Recover files',recovery);expect(fetcher.mock.calls.some(call=>call[0].endsWith('/recover'))).toBe(false);
    (recovery.querySelector('md-outlined-text-field') as any).value='RECOVER';await click('Recover files',recovery);const call=fetcher.mock.calls.find(call=>call[0].endsWith('/recover'));expect(call?.[0]).toBe('/api/v1/engine/compose/projects/project-one/recover');expect(call?.[1].method).toBe('POST');expect((document.querySelector('[data-action="deploy"]') as any).disabled).toBe(false);expect(document.querySelector('[data-action="recover-files"]')).toBeNull();
  });
  it('preserves an open Compose draft and disables Save when pending recovery appears',async()=>{
    const project={id:'project-one',hostId:'host & one',name:'Production',path:'/srv/project'};const fetcher=vi.fn().mockImplementation((path:string)=>Promise.resolve(response(path.endsWith('/projects')?[project]:path.endsWith('/files')?{compose:'services: {}',environment:'NAME=value'}:{...project,pending:{state:'recovery_required'}})));vi.stubGlobal('fetch',fetcher);document.body.append(await composePage());await click('Read and edit files');const editor=document.querySelector('md-dialog')!;const yaml=editor.querySelector('md-outlined-text-field') as any;yaml.value='my unsaved draft';await click('Save',editor);expect(yaml.value).toBe('my unsaved draft');expect(yaml.readOnly).toBe(true);expect((editor.querySelector('[data-action="save"]') as any).disabled).toBe(true);expect(fetcher.mock.calls.some(call=>call[1].method==='PUT')).toBe(false);
  });
  it('does not offer the synthetic local engine as an SSH execution target',()=>{state.hosts=[{id:'local',name:'Local engine',address:'',user:'',port:0,credentialId:''}];state.hostId='local';const targets=hostChoices();expect(targets.node.querySelector('md-checkbox')).toBeNull();expect(targets.node.textContent).toContain('engine-only');expect(()=>targets.read()).toThrow('Choose at least one host');});
  it("tests a stored host with POST and offers explicit key enrollment", async () => {
    const fetcher = vi
      .fn()
      .mockResolvedValue(
        response({
          hostKey: "ssh-ed25519 public-test-key",
          enrollmentRequired: true,
        }),
      );
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await hostsPage(async () => {}));
    await click("Test connection");
    expect(fetcher.mock.calls[0][1].method).toBe("POST");
    expect(fetcher.mock.calls[0][0]).toBe(
      "/api/v1/hosts/host%20%26%20one/test",
    );
    expect(document.querySelector("md-dialog")?.textContent).toContain(
      "Enroll verified key",
    );
    expect(fetcher).toHaveBeenCalledTimes(1);
  });
  it("resource refresh remains pinned to its original host after global selection changes", async () => {
    const fetcher = vi.fn().mockResolvedValue(response([]));
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await enginePage("containers", "original-host"));
    state.hostId = "different-host";
    await click("Refresh");
    expect(fetcher.mock.calls.at(-1)?.[0]).toContain("hostId=original-host");
  });
  it("container deletion uses DELETE on the resource and requires confirmation", async () => {
    const fetcher = vi
      .fn()
      .mockImplementation((_path: string, init: RequestInit) =>
        Promise.resolve(
          init.method === "DELETE"
            ? new Response(null, { status: 204 })
            : response([
                {
                  Id: "container/one",
                  Names: ["<script>bad</script>"],
                  State: "running",
                },
              ]),
        ),
      );
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await enginePage("containers"));
    expect(document.querySelector("script")).toBeNull();
    await click("Remove");
    const d = document.querySelector("md-dialog")!;
    await click("Remove", d);
    expect(fetcher.mock.calls.some((c) => c[1].method === "DELETE")).toBe(
      false,
    );
    const proof = Array.from(d.querySelectorAll("md-outlined-text-field")).find(
      (n: any) => n.label === "Type REMOVE to confirm",
    ) as any;
    proof.value = "REMOVE";
    await click("Remove", d);
    const deletion = fetcher.mock.calls.find((c) => c[1].method === "DELETE");
    expect(deletion?.[0]).toContain("/containers/container%2Fone?");
    expect(deletion?.[0]).not.toContain("/remove");
    expect(deletion?.[0]).toContain("hostId=host+%26+one");
  });
  it("image tag sends repository and tag to the tag action", async () => {
    const fetcher = vi
      .fn()
      .mockImplementation((_path: string, init: RequestInit) =>
        Promise.resolve(
          response(
            init.method === "GET"
              ? [{ Id: "sha256:abc", RepoTags: ["example:old"] }]
              : {},
          ),
        ),
      );
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await enginePage("images"));
    await click("Tag");
    const d = document.querySelector("md-dialog")!;
    const inputs = d.querySelectorAll("md-outlined-text-field") as any;
    inputs[0].value = "example/new";
    inputs[1].value = "stable";
    await click("Save", d);
    const call = fetcher.mock.calls.find((c) => c[0].includes("/tag"));
    expect(call?.[0]).toContain("/images/sha256%3Aabc/tag");
    expect(JSON.parse(call?.[1].body)).toMatchObject({
      repository: "example/new",
      tag: "stable",
      hostId: "host & one",
    });
  });
  it("volume list displays envelope entries", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          response({ Volumes: [{ Name: "volume-one", Driver: "local" }] }),
        ),
    );
    const page = await enginePage("volumes");
    expect(page.textContent).toContain("volume-one");
    expect(page.textContent).toContain("Inspect");
  });
  it("file conflict keeps the original draft and hash", async () => {
    const fetcher = vi
      .fn()
      .mockImplementation((path: string, init: RequestInit) =>
        Promise.resolve(
          init.method === "PUT"
            ? response({ error: "hash mismatch" }, 409)
            : response(
                path.includes("/content")
                  ? { content: "original", hash: "old-hash" }
                  : [
                      {
                        name: "test.txt",
                        path: "/test.txt",
                        directory: false,
                        size: 8,
                      },
                    ],
              ),
        ),
      );
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await filesPage());
    await click("test.txt");
    const d = document.querySelector("md-dialog") as any,
      editor = d.querySelector("md-outlined-text-field") as any;
    editor.value = "my draft";
    await click("Save", d);
    expect(d.open).toBe(true);
    expect(editor.value).toBe("my draft");
    expect(d.textContent).toContain("Your draft is preserved");
    expect(
      JSON.parse(
        fetcher.mock.calls.find((c) => c[1].method === "PUT")?.[1].body,
      ),
    ).toEqual({ content: "my draft", hash: "old-hash" });
  });
  it("saved command run binds the actual immutable revision and selected hosts", async () => {
    const snippet = {
        id: "snippet",
        name: "Maintenance",
        revisions: [{ id: "revision-2", number: 2, command: "printf ok" }],
      },
      fetcher = vi
        .fn()
        .mockImplementation((path: string) =>
          Promise.resolve(
            response(path.endsWith("/snippets") ? [snippet] : { runs: [] }),
          ),
        );
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await commandsPage());
    await click("Run on selected hosts");
    const d = document.querySelector("md-dialog")!;
    await click("Run", d);
    const call = fetcher.mock.calls.find((c) => c[0].endsWith("/runs"));
    expect(JSON.parse(call?.[1].body)).toEqual({
      hostIds: ["host & one"],
      revisionId: "revision-2",
      timeoutSeconds: 600,
    });
  });
  it("new schedule is disabled by default and includes timezone and revision fields", async () => {
    const snippet = {
        id: "s",
        name: "Saved",
        revisions: [{ id: "r", number: 1, command: "echo yes" }],
      },
      fetcher = vi
        .fn()
        .mockImplementation((path: string, init: RequestInit) =>
          Promise.resolve(
            response(
              init.method === "POST"
                ? {}
                : path.endsWith("/snippets")
                  ? [snippet]
                  : [],
            ),
          ),
        );
    vi.stubGlobal("fetch", fetcher);
    document.body.append(await schedulesPage());
    await click("New schedule");
    const d = document.querySelector("md-dialog")!;
    await click("Save", d);
    const call = fetcher.mock.calls.find((c) => c[1].method === "POST");
    expect(JSON.parse(call?.[1].body)).toMatchObject({
      enabled: false,
      revisionId: "r",
      snippetId: "s",
      hostIds: ["host & one"],
      cron: "0 3 * * *",
    });
    expect(JSON.parse(call?.[1].body).timezone).toBeTruthy();
  });
});
