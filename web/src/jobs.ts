import { list, request, segment, url, hostURL } from "./api";
import { state } from "./state";
import {
  button,
  check,
  confirmAction,
  dialog,
  el,
  field,
  panel,
  positiveInteger,
  pre,
  row,
  select,
  stack,
} from "./ui";

const base = "/api/v1/jobs";
export function hostChoices(selected: string[] = [state.hostId]): {
  node: HTMLElement;
  read: () => string[];
} {
  const choices = state.hosts.filter(h=>h.id!=='local').map((h) => ({
    host: h,
    control: check(
      `${h.name} (${h.address || "local"})`,
      selected.includes(h.id),
    ),
  }));
  return {
    node: panel("Target hosts", ...(choices.length?choices.map((c) => c.control.node):[el('p','Add a stored SSH host to run commands. The local host is engine-only.')])),
    read: () => {
      const ids = choices
        .filter((c) => c.control.input.checked)
        .map((c) => c.host.id);
      if (!ids.length) throw Error("Choose at least one host");
      return ids;
    },
  };
}
function revisionChoice(
  snippets: any[],
  initial = "",
): {
  node: HTMLElement;
  read: () => { snippetId: string; revisionId: string };
} {
  const options = snippets.flatMap((s) =>
    list(s.revisions).map((r) => ({
      value: r.id,
      label: `${s.name} · revision ${r.number}`,
      snippet: s.id,
      command: r.command,
    })),
  );
  const selectRevision = select(
      "Saved command revision",
      options,
      initial || options[0]?.value || "",
    ),
    preview = pre("");
  const update = () => {
    preview.textContent =
      options.find((o) => o.value === selectRevision.value)?.command ||
      "No saved revision selected";
  };
  selectRevision.addEventListener("change", update);
  update();
  return {
    node: stack(selectRevision, preview),
    read: () => {
      const option = options.find((o) => o.value === selectRevision.value);
      if (!option)
        throw Error("Create a saved command and choose its revision first");
      return { snippetId: option.snippet, revisionId: option.value };
    },
  };
}
export async function commandsPage(): Promise<HTMLElement> {
  const snippets = list(await request(base + "/snippets")),
    root = stack();
  const refresh = async () => root.replaceWith(await commandsPage());
  const directHost = select(
      "Target host",
      state.hosts
        .filter((h) => h.id !== "local")
        .map((h) => ({ value: h.id, label: h.name })),
      state.hostId === "local" ? "" : state.hostId,
    ),
    directCommand = field("Command", "", "textarea");
  root.append(
    panel(
      "One-time SSH command",
      el(
        "p",
        "This command runs once on the chosen SSH host. Output is discarded. Use a saved revision with explicit retention when output is needed.",
      ),
      directHost,
      directCommand,
      button("Run once", async () => {
        if (!directHost.value || !directCommand.value.trim())
          throw Error("Choose an SSH host and enter a command");
        const result = await request(
          hostURL(directHost.value, "/run"),
          "POST",
          { command: directCommand.value },
        );
        dialog("Execution result", pre(result));
      }),
    ),
  );
  root.append(
    button("New saved command", () => editSnippet(undefined, refresh), true),
  );
  snippets.forEach((snippet) =>
    root.append(
      panel(
        snippet.name,
        el("p", `${snippet.revisions?.length || 0} immutable revisions`),
        row(
          button("Run on selected hosts", () =>
            runDialog(snippets, snippet.revisions?.at(-1)?.id),
          ),
          button("Edit / new revision", () => editSnippet(snippet, refresh)),
          button("View revisions", () =>
            dialog(
              "Command revisions",
              stack(
                ...list(snippet.revisions).map((r) =>
                  panel(
                    `Revision ${r.number} · ${r.createdAt}`,
                    pre(r.command),
                    el(
                      "p",
                      r.retention?.enabled
                        ? `Output retained: up to ${r.retention.maxBytes} bytes, ${r.retention.maxRuns} runs`
                        : "Output is discarded",
                    ),
                  ),
                ),
              ),
            ),
          ),
          button("Delete", () =>
            confirmAction("Delete saved command", snippet.name, async () => {
              await request(
                base + "/snippets/" + segment(snippet.id),
                "DELETE",
              );
              await refresh();
            }),
          ),
        ),
      ),
    ),
  );
  if (!snippets.length)
    root.append(
      el(
        "p",
        "Create a saved command before running it or scheduling it. Each edit creates an immutable revision.",
      ),
    );
  return root;
}
function editSnippet(snippet: any, refresh: () => Promise<void>): void {
  const previous = snippet?.revisions?.at(-1),
    name = field("Name", snippet?.name || ""),
    command = field("Command", previous?.command || "", "textarea"),
    retention = check(
      "Retain encrypted output",
      previous?.retention?.enabled || false,
    ),
    maxBytes = field(
      "Maximum output bytes",
      String(previous?.retention?.maxBytes || 65536),
      "number",
    ),
    maxRuns = field(
      "Retained runs",
      String(previous?.retention?.maxRuns || 10),
      "number",
    );
  dialog(
    snippet ? "New immutable revision" : "New saved command",
    stack(
      name,
      command,
      el(
        "p",
        "Output is discarded by default. Explicit retention stores encrypted, bounded output.",
      ),
      retention.node,
      maxBytes,
      maxRuns,
    ),
    async () => {
      const enabled = retention.input.checked;
      await request(
        base + "/snippets" + (snippet ? "/" + segment(snippet.id) : ""),
        snippet ? "PUT" : "POST",
        {
          name: name.value,
          command: command.value,
          retention: {
            enabled,
            ...(enabled
              ? {
                  maxBytes: positiveInteger(
                    maxBytes.value,
                    "Maximum bytes",
                    1048576,
                  ),
                  maxRuns: positiveInteger(
                    maxRuns.value,
                    "Retained runs",
                    1000,
                  ),
                }
              : {}),
          },
        },
      );
      await refresh();
    },
  );
}
function runDialog(snippets: any[], initial = ""): void {
  const revision = revisionChoice(snippets, initial),
    hosts = hostChoices(),
    timeout = field("Timeout seconds", "600", "number");
  dialog(
    "Run saved revision",
    stack(revision.node, hosts.node, timeout),
    async () => {
      const result = await request(base + "/runs", "POST", {
        hostIds: hosts.read(),
        revisionId: revision.read().revisionId,
        timeoutSeconds: positiveInteger(timeout.value, "Timeout", 600),
      });
      dialog("Accepted runs", pre(result));
    },
    "Run",
  );
}
export async function schedulesPage(): Promise<HTMLElement> {
  const schedules = list(await request(base + "/schedules")),
    snippets = list(await request(base + "/snippets")),
    root = stack();
  const refresh = async () => root.replaceWith(await schedulesPage());
  const edit = (schedule?: any) => {
    const revision = revisionChoice(snippets, schedule?.revisionId),
      hosts = hostChoices(
        schedule?.hostIds || [schedule?.hostId || state.hostId],
      ),
      cron = field("Five-field cron", schedule?.cron || "0 3 * * *"),
      timezone = field(
        "IANA timezone",
        schedule?.timezone ||
          Intl.DateTimeFormat().resolvedOptions().timeZone ||
          "America/Toronto",
      ),
      enabled = check(
        "Enable unattended AUTOAPPROVED execution",
        schedule?.enabled || false,
      ),
      preview = pre("Preview next run times before enabling.");
    dialog(
      schedule ? "Edit schedule" : "Create schedule",
      stack(
        el(
          "p",
          "An enabled schedule runs its exact saved revision on these hosts without another confirmation. Missed runs are not replayed and overlapping runs are skipped.",
        ),
        revision.node,
        hosts.node,
        cron,
        timezone,
        enabled.node,
        button("Preview next five occurrences", async () => {
          preview.textContent = JSON.stringify(
            await request(base + "/schedules/preview", "POST", {
              cron: cron.value,
              timezone: timezone.value,
              after: new Date().toISOString(),
              count: 5,
            }),
            null,
            2,
          );
        }),
        preview,
      ),
      async () => {
        await request(
          base + "/schedules" + (schedule ? "/" + segment(schedule.id) : ""),
          schedule ? "PUT" : "POST",
          {
            ...revision.read(),
            hostIds: hosts.read(),
            cron: cron.value,
            timezone: timezone.value,
            enabled: enabled.input.checked,
          },
        );
        await refresh();
      },
    );
  };
  root.append(button("New schedule", () => edit(), true));
  schedules.forEach((schedule) =>
    root.append(
      panel(
        schedule.cron,
        el(
          "p",
          `${schedule.timezone} · ${schedule.enabled ? "Enabled · AUTOAPPROVED" : "Disabled"}`,
        ),
        el(
          "p",
          `Hosts: ${(schedule.hostIds || [schedule.hostId]).map((id: string) => state.hosts.find((h) => h.id === id)?.name || id).join(", ")}`,
        ),
        el("code", `Revision: ${schedule.revisionId}`),
        row(
          button("Edit", () => edit(schedule)),
          button(schedule.enabled ? "Disable" : "Enable", async () => {
            await request(base + "/schedules/" + segment(schedule.id), "PUT", {
              hostIds: schedule.hostIds || [schedule.hostId],
              snippetId: schedule.snippetId,
              revisionId: schedule.revisionId,
              cron: schedule.cron,
              timezone: schedule.timezone,
              enabled: !schedule.enabled,
            });
            await refresh();
          }),
          button("Delete", () =>
            confirmAction("Delete schedule", schedule.id, async () => {
              await request(
                base + "/schedules/" + segment(schedule.id),
                "DELETE",
              );
              await refresh();
            }),
          ),
        ),
      ),
    ),
  );
  if (!schedules.length) root.append(el("p", "No schedules stored."));
  return root;
}
export async function runsPage(): Promise<HTMLElement> {
  const root = stack(),
    filter = select(
      "Run host",
      [
        { value: "", label: "All hosts" },
        ...state.hosts.map((h) => ({ value: h.id, label: h.name })),
      ],
      "",
    ),
    records = stack();
  const refresh = async () => {
    const runs = list(
      await request(url(base + "/runs", { hostId: filter.value || undefined })),
    );
    records.replaceChildren();
    runs.forEach((run) => {
      const id = segment(run.id);
      records.append(
        panel(
          `${state.hosts.find((h) => h.id === run.hostId)?.name || run.hostId} · ${run.status}`,
          el("code", run.id),
          el(
            "p",
            `${run.startedAt} · ${run.intent || run.source} · exit ${run.exitCode ?? "unavailable"}`,
          ),
          row(
            button("Details", async () =>
              dialog("Run details", pre(await request(base + "/runs/" + id))),
            ),
            button("Retained output", async () =>
              dialog(
                "Retained output",
                pre(await request(base + "/runs/" + id + "/output")),
              ),
            ),
            button("Request cancellation", async () => {
              await request(base + "/runs/" + id + "/cancel", "POST");
              await refresh();
            }),
          ),
        ),
      );
    });
    if (!runs.length) records.append(el("p", "No runs recorded."));
  };
  filter.addEventListener("change", () => {
    refresh().catch((error) => {
      records.replaceChildren(el("p", error.message));
    });
  });
  root.append(
    row(
      filter,
      button("Refresh runs", refresh),
      button("Audit events", async () =>
        dialog("Audit events", pre(await request(base + "/audit?limit=100"))),
      ),
    ),
    records,
  );
  await refresh();
  return root;
}
