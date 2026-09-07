import { request, list, hostURL, url, segment, APIError } from "./api";
import { state, Host, selectedHost } from "./state";
import {
  button,
  check,
  confirmAction,
  dialog,
  el,
  field,
  notify,
  panel,
  positiveInteger,
  pre,
  row,
  select,
  stack,
  values,
} from "./ui";

export async function hostsPage(
  onChange: () => Promise<void>,
): Promise<HTMLElement> {
  const root = stack(),
    search = field("Filter hosts"),
    records = stack();
  const refresh = async () => {
    await onChange();
    root.replaceWith(await hostsPage(onChange));
  };
  const draw = () => {
    records.replaceChildren();
    state.hosts
      .filter((h) =>
        JSON.stringify(h).toLowerCase().includes(search.value.toLowerCase()),
      )
      .forEach((h) => {
        records.append(
          panel(
            h.name,
            el(
              "p",
              h.id === "local"
                ? "Local engine"
                : `${h.user}@${h.address}:${h.port} · ${h.group || "Ungrouped"}`,
            ),
            el("p", (h.tags || []).join(", ")),
            row(
              button("Select host", async () => {
                state.hostId = h.id;
                await onChange();
              }),
              h.id !== "local"
                ? button("Edit", () => editHost(h, refresh))
                : undefined,
              h.id !== "local"
                ? button("Test connection", async () => {
                    const result = await request(
                      hostURL(h.id, "/test"),
                      "POST",
                    );
                    if (result.connected) {
                      dialog("Connection test", pre(result));
                      return;
                    }
                    if (result.hostKey)
                      dialog(
                        "Verify and enroll host key",
                        stack(
                          el(
                            "p",
                            "Verify this public host key through a trusted independent channel before accepting it.",
                          ),
                          pre(result.hostKey),
                        ),
                        async () => {
                          await request(
                            hostURL(h.id, "/enroll-host-key"),
                            "POST",
                            { hostKey: result.hostKey },
                          );
                          await refresh();
                        },
                        "Enroll verified key",
                      );
                    else dialog("Connection test", pre(result));
                  })
                : undefined,
              h.id !== "local"
                ? button("Delete", () =>
                    confirmAction("Delete stored host", h.name, async () => {
                      await request(hostURL(h.id), "DELETE");
                      await refresh();
                    }),
                  )
                : undefined,
            ),
          ),
        );
      });
  };
  search.addEventListener("input", draw);
  draw();
  const upload = el("input");
  upload.type = "file";
  upload.accept = "application/json,.json";
  upload.setAttribute("aria-label", "Import host records JSON");
  upload.addEventListener("change", async () => {
    try {
      const file = upload.files?.[0];
      if (!file) return;
      if (file.size > 1048576) throw Error("Host import must be at most 1 MiB");
      const rows = JSON.parse(await file.text());
      if (!Array.isArray(rows) || rows.length > 100)
        throw Error("Import must be an array of at most 100 hosts");
      rows.forEach((h: any) => {
        if (
          !h ||
          typeof h.name !== "string" ||
          typeof h.address !== "string" ||
          typeof h.user !== "string" ||
          typeof h.credentialId !== "string" ||
          Object.keys(h).some(
            (k) =>
              ![
                "name",
                "address",
                "port",
                "user",
                "credentialId",
                "group",
                "tags",
                "jumpIds",
              ].includes(k),
          )
        )
          throw Error(
            "Invalid host record or unexpected field. Imports accept metadata only, without keys or secrets.",
          );
        positiveInteger(String(h.port || 22), "Port");
      });
      dialog(
        "Import host records",
        el(
          "p",
          `Create ${rows.length} hosts using existing credential identifiers? Host keys are verified separately.`,
        ),
        async () => {
          let added = 0;
          try {
            for (const h of rows) {
              await request("/api/v1/hosts", "POST", {
                ...h,
                port: h.port || 22,
              });
              added++;
            }
          } finally {
            notify(`${added} of ${rows.length} host records imported`);
            await refresh();
          }
        },
        "Import",
      );
    } catch (error) {
      notify((error as Error).message, true);
    } finally {
      upload.value = "";
    }
  });
  root.append(
    row(
      button("Add host", () => editHost(undefined, refresh), true),
      button("Manage credentials", async () =>
        dialog("Credentials", await credentialsPage()),
      ),
      el("label", "Import metadata: "),
      upload,
    ),
    search,
    records,
  );
  return root;
}
async function editHost(
  host: Host | undefined,
  refresh: () => Promise<void>,
): Promise<void> {
  const credentials = list(await request("/api/v1/credentials")),
    name = field("Name", host?.name || ""),
    address = field("Address", host?.address || ""),
    port = field("SSH port", String(host?.port || 22), "number"),
    user = field("SSH user", host?.user || ""),
    group = field("Group", host?.group || ""),
    tags = field(
      "Tags, one per line",
      host?.tags?.join("\n") || "",
      "textarea",
    ),
    credential = select(
      "Stored credential",
      credentials.map((c) => ({ value: c.id, label: `${c.name} · ${c.kind}` })),
      host?.credentialId || "",
    ),
    jumps = stack();
  const ordered: string[] = [...(host?.jumpIds || [])];
  const available = select(
    "Jump host",
    state.hosts
      .filter((h) => h.id !== host?.id && h.id !== "local")
      .map((h) => ({ value: h.id, label: h.name })),
  );
  const draw = () => {
    jumps.replaceChildren(
      ...ordered.map((id, index) =>
        row(
          el(
            "span",
            `${index + 1}. ${state.hosts.find((h) => h.id === id)?.name || id}`,
          ),
          button("Move up", () => {
            if (index > 0) {
              [ordered[index - 1], ordered[index]] = [
                ordered[index],
                ordered[index - 1],
              ];
              draw();
            }
          }),
          button("Remove hop", () => {
            ordered.splice(index, 1);
            draw();
          }),
        ),
      ),
    );
  };
  draw();
  dialog(
    host ? "Edit host" : "Add host",
    stack(
      name,
      address,
      port,
      user,
      credential,
      group,
      tags,
      panel(
        "Ordered jump chain",
        row(
          available,
          button("Add hop", () => {
            if (!available.value || ordered.includes(available.value))
              throw Error("Choose a new jump host");
            ordered.push(available.value);
            draw();
          }),
        ),
        jumps,
      ),
    ),
    async () => {
      if (!credential.value)
        throw Error("Create and choose a stored credential");
      await request(
        "/api/v1/hosts" + (host ? "/" + segment(host.id) : ""),
        host ? "PUT" : "POST",
        {
          name: name.value,
          address: address.value,
          port: positiveInteger(port.value, "SSH port"),
          user: user.value,
          credentialId: credential.value,
          group: group.value,
          tags: values(tags.value),
          jumpIds: ordered,
          ...(host?.hostKey ? { hostKey: host.hostKey } : {}),
        },
      );
      await refresh();
    },
  );
}
async function credentialsPage(): Promise<HTMLElement> {
  const root = stack(),
    records = list(await request("/api/v1/credentials"));
  const refresh = async () => root.replaceWith(await credentialsPage());
  const edit = (record?: any) => {
    const name = field("Credential name", record?.name || ""),
      kind = select(
        "Kind",
        [
          { value: "password", label: "Password" },
          { value: "privateKey", label: "Private key" },
        ],
        record?.kind || "password",
      ),
      secret = field(
        record ? "Replacement secret (leave blank to keep)" : "Secret",
        "",
        record?.kind === "privateKey" ? "textarea" : "password",
      );
    kind.addEventListener("change", () => {
      secret.type = kind.value === "privateKey" ? "textarea" : "password";
      secret.rows = 8;
    });
    if (record) kind.disabled = true;
    dialog(
      record ? "Update credential" : "Create credential",
      stack(
        el(
          "p",
          "Secrets are write-only. Existing values are never returned or shown.",
        ),
        name,
        kind,
        secret,
      ),
      async () => {
        try {
          if (!record && !secret.value) throw Error("Secret is required");
          await request(
            "/api/v1/credentials" + (record ? "/" + segment(record.id) : ""),
            record ? "PUT" : "POST",
            {
              name: name.value,
              ...(!record ? { kind: kind.value } : {}),
              ...(secret.value ? { secret: secret.value } : {}),
            },
          );
          await refresh();
        } finally {
          secret.value = "";
        }
      },
    );
  };
  root.append(button("Create credential", () => edit(), true));
  records.forEach((record) =>
    root.append(
      panel(
        record.name,
        el("p", record.kind),
        row(
          button("Update", () => edit(record)),
          button("Delete", () =>
            confirmAction("Delete credential", record.name, async () => {
              await request(
                "/api/v1/credentials/" + segment(record.id),
                "DELETE",
              );
              await refresh();
            }),
          ),
        ),
      ),
    ),
  );
  return root;
}
export async function filesPage(
  hostId = selectedHost().id,
): Promise<HTMLElement> {
  const root = stack(),
    path = field("Remote directory", "/"),
    entries = stack(),
    filter = field("Filter directory entries");
  let loaded: any[] = [];
  const join = (name: string) => path.value.replace(/\/$/, "") + "/" + name;
  const browse = async () => {
    loaded = list(
      await request(
        url(hostURL(hostId, "/files"), { path: path.value || "/" }),
      ),
    );
    draw();
  };
  const draw = () => {
    entries.replaceChildren();
    loaded
      .filter((entry) =>
        entry.name.toLowerCase().includes(filter.value.toLowerCase()),
      )
      .forEach((entry) => {
        const target = entry.path || join(entry.name),
          directory = entry.isDir || entry.directory;
        entries.append(
          row(
            button(entry.name, async () => {
              if (directory) {
                path.value = target;
                await browse();
              } else await editFile(hostId, target);
            }),
            el("span", directory ? "Directory" : `${entry.size ?? 0} bytes`),
            !directory
              ? button("Download", () => downloadFile(hostId, target))
              : undefined,
          ),
        );
      });
    if (!entries.childElementCount)
      entries.append(el("p", "No entries match."));
  };
  const upload = el("input");
  upload.type = "file";
  upload.setAttribute("aria-label", "Upload a file to the displayed directory");
  upload.addEventListener("change", async () => {
    const file = upload.files?.[0];
    if (!file) return;
    try {
      if (file.size > 33554432) throw Error("Upload limit is 32 MiB");
      const target = join(file.name);
      dialog(
        "Upload file",
        el(
          "p",
          `Write ${file.name} to ${target}? Existing content may be replaced.`,
        ),
        async () => {
          await request(
            url(hostURL(hostId, "/files/upload"), { path: target }),
            "POST",
            file,
            true,
          );
          await browse();
        },
        "Upload",
      );
    } catch (error) {
      notify((error as Error).message, true);
    } finally {
      upload.value = "";
    }
  });
  filter.addEventListener("input", draw);
  root.append(
    row(
      path,
      button("Browse", browse, true),
      button("Parent directory", async () => {
        path.value =
          path.value.replace(/\/$/, "").replace(/\/[^/]*$/, "") || "/";
        await browse();
      }),
    ),
    row(el("label", "Upload file: "), upload),
    filter,
    entries,
  );
  await browse();
  return root;
}
async function editFile(hostId: string, path: string): Promise<void> {
  const endpoint = url(hostURL(hostId, "/files/content"), { path }),
    current = await request(endpoint),
    content = field("Text content", current.content, "textarea"),
    status = el("p", "Writes require the original content hash.");
  content.rows = 18;
  let hash = current.hash;
  dialog(
    path,
    stack(
      status,
      content,
      button("Reload current server content", async () => {
        dialog(
          "Discard editor changes",
          el(
            "p",
            "Replace unsaved editor content with the current server file?",
          ),
          async () => {
            const latest = await request(endpoint);
            content.value = latest.content;
            hash = latest.hash;
            status.textContent = "Current server content loaded.";
          },
          "Reload",
        );
      }),
    ),
    async () => {
      try {
        const result = await request(endpoint, "PUT", {
          content: content.value,
          hash,
        });
        hash = result.hash;
      } catch (error) {
        if (error instanceof APIError && error.status === 409)
          status.textContent =
            "Conflict: the server file changed. Your draft is preserved. Copy your changes or reload the current version before retrying.";
        throw error;
      }
    },
  );
}
async function downloadFile(hostId: string, path: string): Promise<void> {
  const response = await fetch(
    url(hostURL(hostId, "/files/download"), { path }),
    { credentials: "same-origin" },
  );
  if (!response.ok) throw Error(`Download failed: HTTP ${response.status}`);
  const blob = await response.blob(),
    href = URL.createObjectURL(blob),
    link = el("a");
  link.href = href;
  link.download = path.split("/").at(-1) || "download";
  link.click();
  setTimeout(() => URL.revokeObjectURL(href), 60000);
}
export async function tunnelsPage(
  hostId = selectedHost().id,
): Promise<HTMLElement> {
  const root = stack(),
    tunnels = list(await request("/api/v1/tunnels")).filter(
      (t) => t.hostId === hostId,
    ),
    refresh = async () => root.replaceWith(await tunnelsPage(hostId));
  root.append(
    button(
      "Start tunnel",
      () => {
        const direction = select(
            "Direction",
            [
              { value: "local", label: "Local forwarding" },
              { value: "reverse", label: "Reverse forwarding" },
            ],
            "local",
          ),
          listen = field("Listen address", "127.0.0.1"),
          listenPort = field("Listen port", "", "number"),
          target = field("Target address", "127.0.0.1"),
          targetPort = field("Target port", "", "number");
        dialog(
          "Start tunnel",
          stack(direction, listen, listenPort, target, targetPort),
          async () => {
            await request("/api/v1/tunnels", "POST", {
              hostId,
              direction: direction.value,
              listenAddress: listen.value,
              listenPort: positiveInteger(listenPort.value, "Listen port"),
              targetAddress: target.value,
              targetPort: positiveInteger(targetPort.value, "Target port"),
            });
            await refresh();
          },
          "Start",
        );
      },
      true,
    ),
    button("Refresh", refresh),
  );
  tunnels.forEach((t) =>
    root.append(
      panel(
        `${t.direction} · ${t.state}`,
        el(
          "p",
          `${t.listenAddress}:${t.listenPort} → ${t.targetAddress}:${t.targetPort}`,
        ),
        button("Stop tunnel", () =>
          confirmAction("Stop tunnel", t.id, async () => {
            await request("/api/v1/tunnels/" + segment(t.id), "DELETE");
            await refresh();
          }),
        ),
      ),
    ),
  );
  if (!tunnels.length) root.append(el("p", "No active tunnels for this host."));
  return root;
}
