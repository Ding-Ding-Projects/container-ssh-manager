import { request, url, engineURL, list, segment } from "./api";
import { state, selectedHost } from "./state";
import {
  button,
  check,
  dialog,
  confirmAction,
  el,
  field,
  objectJSON,
  pairs,
  panel,
  pre,
  row,
  select,
  stack,
  values,
  notify,
} from "./ui";

export function showResult(title: string, result: unknown): void {
  dialog(title, pre(result));
}
export function watchOperation(operation: any): void {
  if (!operation?.id) {
    showResult("Operation result", operation);
    return;
  }
  const output = pre(operation),
    base = "/api/v1/engine/operations/" + segment(operation.id);
  let timer: ReturnType<typeof setTimeout> | undefined,
    closed = false,
    polls = 0;
  const refresh = async () => {
    const current = await request(base);
    output.textContent = JSON.stringify(current, null, 2);
    return current;
  };
  const d = dialog(
    "Engine operation",
    stack(
      output,
      row(
        button("Refresh status", async () => {
          await refresh();
        }),
        button("Request cancellation", async () => {
          output.textContent = JSON.stringify(
            await request(base + "/cancel", "POST"),
            null,
            2,
          );
        }),
      ),
    ),
  );
  d.addEventListener(
    "closed",
    () => {
      closed = true;
      clearTimeout(timer);
    },
    { once: true },
  );
  const poll = async () => {
    if (closed) return;
    try {
      const current = await refresh();
      if (
        !closed &&
        ["queued", "running", "canceling"].includes(current.state) &&
        ++polls < 120
      )
        timer = setTimeout(poll, Math.min(1000 + polls * 250, 10000));
    } catch (error) {
      notify((error as Error).message, true);
    }
  };
  if (["queued", "running", "canceling"].includes(operation.state))
    timer = setTimeout(poll, 1000);
}
export async function enginePage(
  kind: string,
  hostId = selectedHost().id,
): Promise<HTMLElement> {
  const response = await request(url(engineURL(kind), { hostId, all: true }));
  const records = list(response, kind === "volumes" ? "Volumes" : undefined);
  const container = stack();
  const refresh = async () =>
    container.replaceWith(await enginePage(kind, hostId));
  const search = field("Filter resources");
  const grid = el("div", "", "records");
  const draw = () => {
    grid.replaceChildren();
    records
      .filter((x) =>
        JSON.stringify(x).toLowerCase().includes(search.value.toLowerCase()),
      )
      .forEach((x) => {
        const id = String(x.Id || x.ID || x.Name || x.id || x.name);
        const name =
          x.Names?.join(", ") || x.RepoTags?.join(", ") || x.Name || id;
        const actions = row();
        if (kind !== "images")
          actions.append(
            button("Inspect", async () =>
              showResult(
                "Resource inspection",
                await request(url(engineURL(kind, id), { hostId })),
              ),
            ),
          );
        if (kind === "containers") {
          ["start", "stop", "restart"].forEach((action) =>
            actions.append(
              button(action[0].toUpperCase() + action.slice(1), async () => {
                await request(engineURL(kind, id, action), "POST", { hostId });
                await refresh();
              }),
            ),
          );
          actions.append(
            button("Recreate", () => {
              const pull = check("Pull current image before recreating");
              dialog(
                "Recreate container",
                stack(
                  el(
                    "p",
                    `Recreate ${name}. The server preserves inspected configuration and reports recovery state.`,
                  ),
                  pull.node,
                ),
                async () =>
                  watchOperation(
                    await request(engineURL(kind, id, "recreate"), "POST", {
                      hostId,
                      pull: pull.input.checked,
                    }),
                  ),
                "Recreate",
              );
            }),
          );
          actions.append(
            button("Logs", () => {
              const tail = field("Tail lines", "200", "number"),
                timestamps = check("Include timestamps", true),
                output = pre("Select Load to read logs.");
              dialog(
                "Container logs",
                stack(
                  row(
                    tail,
                    timestamps.node,
                    button("Load", async () => {
                      output.textContent = await request(
                        url(engineURL(kind, id, "logs"), {
                          hostId,
                          stdout: true,
                          stderr: true,
                          tail: tail.value,
                          timestamps: timestamps.input.checked,
                        }),
                      );
                    }),
                  ),
                  output,
                ),
              );
            }),
            button("Stats", async () =>
              showResult(
                "Container statistics",
                await request(
                  url(engineURL(kind, id, "stats"), { hostId, stream: false }),
                ),
              ),
            ),
            button("Execute", () => {
              const cmd = field(
                  "Arguments, one per line",
                  "/bin/sh\n-c\nprintf hello",
                  "textarea",
                ),
                env = field("Environment, KEY=VALUE per line", "", "textarea"),
                dir = field("Working directory"),
                user = field("User");
              dialog(
                "Execute in container",
                stack(cmd, env, dir, user),
                async () =>
                  showResult(
                    "Execution result",
                    await request(engineURL(kind, id, "exec"), "POST", {
                      hostId,
                      cmd: values(cmd.value),
                      env: values(env.value),
                      workingDir: dir.value,
                      user: user.value,
                      tty: false,
                    }),
                  ),
                "Execute",
              );
            }),
          );
        }
        if (kind === "images")
          actions.append(
            button("Tag", () => {
              const repository = field("Repository"),
                tag = field("Tag", "latest");
              dialog("Tag image", stack(repository, tag), async () => {
                await request(engineURL(kind, id, "tag"), "POST", {
                  hostId,
                  repository: repository.value,
                  tag: tag.value,
                });
                await refresh();
              });
            }),
          );
        if (kind === "networks")
          ["connect", "disconnect"].forEach((action) =>
            actions.append(
              button(
                action === "connect"
                  ? "Connect container"
                  : "Disconnect container",
                async () => {
                  const containers = list(
                      await request(
                        url(engineURL("containers"), { hostId, all: true }),
                      ),
                    ),
                    choice = select(
                      "Container",
                      containers.map((c) => ({
                        value: c.Id,
                        label: c.Names?.join(", ") || c.Id,
                      })),
                    );
                  const force = check("Force disconnect");
                  dialog(
                    action === "connect"
                      ? "Connect container"
                      : "Disconnect container",
                    stack(
                      choice,
                      action === "disconnect" ? force.node : undefined,
                    ),
                    async () => {
                      if (!choice.value) throw Error("Choose a container");
                      await request(engineURL(kind, id, action), "POST", {
                        hostId,
                        container: choice.value,
                        force: force.input.checked,
                      });
                      await refresh();
                    },
                  );
                },
              ),
            ),
          );
        actions.append(
          button("Remove", () => {
            const force = check("Force removal"),
              volumes = check("Also remove anonymous volumes");
            const proof = field("Type REMOVE to confirm");
            dialog(
              "Remove resource",
              stack(
                el("p", `Remove ${name}. This changes live resources.`),
                force.node,
                kind === "containers" ? volumes.node : undefined,
                proof,
              ),
              async () => {
                if (proof.value !== "REMOVE")
                  throw Error("Confirmation must be REMOVE");
                await request(
                  url(engineURL(kind, id), {
                    hostId,
                    force: force.input.checked,
                    volumes: volumes.input.checked,
                  }),
                  "DELETE",
                );
                await refresh();
              },
              "Remove",
            );
          }),
        );
        grid.append(
          panel(
            String(name),
            el("p", String(x.Status || x.State || x.Driver || x.Size || "")),
            el("code", id),
            actions,
          ),
        );
      });
    if (!grid.childElementCount) grid.append(el("p", "No resources match."));
  };
  search.addEventListener("input", draw);
  draw();
  container.append(
    row(
      button("Refresh", refresh),
      button(
        kind === "images" ? "Pull image" : "Create",
        () => createResource(kind, hostId, refresh),
        true,
      ),
      kind === "images"
        ? button("Build image", () => buildImage(hostId))
        : undefined,
    ),
    search,
    grid,
  );
  return container;
}
function createResource(
  kind: string,
  hostId: string,
  refresh: () => Promise<void>,
): void {
  const name = field(kind === "images" ? "Image reference" : "Name"),
    driver = field("Driver", kind === "volumes" ? "local" : "bridge"),
    labels = field("Labels, KEY=VALUE per line", "", "textarea");
  if (kind === "images") {
    const platform = field("Platform (optional)");
    dialog(
      "Pull image",
      stack(name, platform),
      async () =>
        watchOperation(
          await request(engineURL(kind, undefined, "pull"), "POST", {
            hostId,
            reference: name.value,
            platform: platform.value,
          }),
        ),
      "Pull",
    );
    return;
  }
  if (kind === "containers") {
    const image = field("Image reference"),
      command = field("Command arguments, one per line", "", "textarea"),
      env = field("Environment, KEY=VALUE per line", "", "textarea"),
      ports = field(
        "Published ports, host:container/protocol per line",
        "",
        "textarea",
      ),
      binds = field("Mounts, source:destination[:ro] per line", "", "textarea"),
      restart = select(
        "Restart policy",
        ["no", "always", "unless-stopped", "on-failure"].map((value) => ({
          value,
          label: value,
        })),
        "no",
      ),
      network = field("Network mode", "bridge"),
      advanced = field("Additional HostConfig JSON", "{}", "textarea");
    dialog(
      "Create container",
      stack(
        name,
        image,
        command,
        env,
        ports,
        binds,
        restart,
        network,
        labels,
        advanced,
      ),
      async () => {
        if (!image.value.trim()) throw Error("Image is required");
        const portBindings: Record<string, unknown[]> = {},
          exposed: Record<string, {}> = {};
        values(ports.value).forEach((item) => {
          const match = /^(\d+):(\d+)(?:\/(tcp|udp|sctp))?$/.exec(item);
          if (
            !match ||
            Number(match[1]) > 65535 ||
            Number(match[2]) > 65535 ||
            Number(match[1]) < 1 ||
            Number(match[2]) < 1
          )
            throw Error(
              "Ports require host:container/protocol with ports 1 to 65535",
            );
          const key = match[2] + "/" + (match[3] || "tcp");
          portBindings[key] = [{ HostIp: "", HostPort: match[1] }];
          exposed[key] = {};
        });
        await request(engineURL(kind), "POST", {
          hostId,
          name: name.value,
          config: {
            Image: image.value,
            Cmd: values(command.value),
            Env: values(env.value),
            Labels: pairs(labels.value),
            ExposedPorts: exposed,
          },
          hostConfig: {
            ...objectJSON(advanced.value, "HostConfig"),
            Binds: values(binds.value),
            PortBindings: portBindings,
            RestartPolicy: { Name: restart.value },
            NetworkMode: network.value,
          },
        });
        await refresh();
      },
      "Create",
    );
    return;
  }
  const options = field("Driver options, KEY=VALUE per line", "", "textarea"),
    internal = check("Internal network"),
    attachable = check("Attachable network");
  dialog(
    "Create " + kind,
    stack(
      name,
      driver,
      labels,
      options,
      kind === "networks" ? row(internal.node, attachable.node) : undefined,
    ),
    async () => {
      await request(engineURL(kind), "POST", {
        hostId,
        name: name.value,
        driver: driver.value,
        labels: pairs(labels.value),
        ...(kind === "volumes"
          ? { driverOpts: pairs(options.value) }
          : {
              options: pairs(options.value),
              internal: internal.input.checked,
              attachable: attachable.input.checked,
            }),
      });
      await refresh();
    },
    "Create",
  );
}
function buildImage(hostId: string): void {
  const context = field("Absolute host build context path"),
    dockerfile = field("Dockerfile path", "Dockerfile"),
    tags = field("Tags, one per line", "", "textarea"),
    args = field("Build arguments, KEY=VALUE per line", "", "textarea");
  dialog(
    "Build image",
    stack(context, dockerfile, tags, args),
    async () =>
      watchOperation(
        await request(engineURL("images", undefined, "build"), "POST", {
          hostId,
          contextPath: context.value,
          dockerfile: dockerfile.value,
          tags: values(tags.value),
          buildArgs: pairs(args.value),
        }),
      ),
    "Build",
  );
}

export async function composePage(
  hostId = selectedHost().id,
): Promise<HTMLElement> {
  const root = stack(),
    base = "/api/v1/engine/compose/projects",
    projects = list(await request(base)).filter((p) => p.hostId === hostId);
  const refresh = async () => root.replaceWith(await composePage(hostId));
  root.append(
    button(
      "Create or adopt project",
      () => {
        const name = field("Project name"),
          path = field("Absolute host project directory"),
          adopt = check("Adopt existing compose.yaml and .env files");
        dialog("Compose project", stack(name, path, adopt.node), async () => {
          await request(base, "POST", {
            hostId,
            name: name.value,
            path: path.value,
            adopt: adopt.input.checked,
          });
          await refresh();
        });
      },
      true,
    ),
  );
  projects.forEach((p) => {
    const path = base + "/" + segment(p.id);
    root.append(
      panel(
        p.name,
        el("p", p.path),
        row(
          button("Read and edit files", async () => {
            const current = await request(path + "/files"),
              yaml = field("Compose YAML", current.compose || "", "textarea"),
              env = field(
                "Environment file",
                current.environment || "",
                "textarea",
              );
            yaml.rows = 16;
            env.rows = 8;
            dialog(
              "Compose files",
              stack(
                el(
                  "p",
                  "Environment values are sensitive. Files remain in this editor only and are cleared when it closes.",
                ),
                yaml,
                env,
              ),
              async () => {
                await request(path + "/files", "PUT", {
                  compose: yaml.value,
                  environment: env.value,
                });
                yaml.value = "";
                env.value = "";
                await refresh();
              },
            );
          }),
          button("Revisions", async () => {
            const project = await request(path);
            dialog(
              "Compose revisions",
              stack(
                ...list(project.revisions).map((r) =>
                  row(
                    el("span", `Revision ${r.number} · ${r.createdAt}`),
                    button("Restore", () =>
                      confirmAction(
                        "Restore Compose files",
                        "revision " + r.number,
                        async () => {
                          await request(
                            path + "/restore/" + r.number,
                            "POST",
                            {},
                          );
                          await refresh();
                        },
                      ),
                    ),
                  ),
                ),
              ),
            );
          }),
          button("Validate", async () =>
            showResult(
              "Compose validation",
              await request(path + "/validate", "POST", {}),
            ),
          ),
          button("Deploy", () => {
            const pull = check("Pull images", true),
              build = check("Build images"),
              detach = check("Run detached", true);
            dialog(
              "Deploy Compose project",
              stack(pull.node, build.node, detach.node),
              async () =>
                showResult(
                  "Compose deployment",
                  await request(path + "/deploy", "POST", {
                    pull: pull.input.checked,
                    build: build.input.checked,
                    detach: detach.input.checked,
                  }),
                ),
              "Deploy",
            );
          }),
          button("Stop", () =>
            dialog(
              "Stop Compose project",
              el("p", `Stop services in ${p.name}?`),
              async () =>
                showResult(
                  "Compose stop",
                  await request(path + "/stop", "POST", {}),
                ),
              "Stop",
            ),
          ),
          button("Down", () => {
            const volumes = check("Remove volumes"),
              images = select("Remove images", [
                { value: "", label: "Keep images" },
                { value: "local", label: "Local images" },
                { value: "all", label: "All service images" },
              ]);
            const proof = field("Type REMOVE to confirm");
            dialog(
              "Remove Compose resources",
              stack(volumes.node, images, proof),
              async () => {
                if (proof.value !== "REMOVE")
                  throw Error("Confirmation must be REMOVE");
                showResult(
                  "Compose down",
                  await request(path + "/down", "POST", {
                    volumes: volumes.input.checked,
                    images: images.value,
                  }),
                );
              },
              "Remove",
            );
          }),
        ),
      ),
    );
  });
  if (!projects.length) root.append(el("p", "No projects on this host."));
  return root;
}
