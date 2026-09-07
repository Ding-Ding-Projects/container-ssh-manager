export class APIError extends Error {
  constructor(
    message: string,
    public status: number,
  ) {
    super(message);
  }
}
export const segment = (value: string) => encodeURIComponent(value);
export function url(
  path: string,
  params: Record<string, string | number | boolean | undefined> = {},
): string {
  const query = new URLSearchParams();
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined) query.set(key, String(value));
  });
  return path + (query.size ? "?" + query : "");
}
export async function request<T = any>(
  path: string,
  method = "GET",
  body?: unknown,
  raw = false,
): Promise<T> {
  const response = await fetch(path, {
    method,
    credentials: "same-origin",
    headers:
      body !== undefined && !raw ? { "Content-Type": "application/json" } : {},
    body:
      body === undefined
        ? undefined
        : raw
          ? (body as BodyInit)
          : JSON.stringify(body),
  });
  if (!response.ok) {
    const data = await response
      .json()
      .catch(() => ({ error: response.statusText }));
    throw new APIError(
      data.error || `HTTP ${response.status}`,
      response.status,
    );
  }
  if (response.status === 204) return undefined as T;
  const type = response.headers.get("content-type") || "";
  return (
    type.includes("json") ? await response.json() : await response.text()
  ) as T;
}
export function list<T = any>(response: unknown, key?: string): T[] {
  const value =
    key && response && typeof response === "object"
      ? (response as any)[key]
      : response;
  if (value === null || value === undefined) return [];
  if (!Array.isArray(value)) throw Error("Server returned an unexpected list");
  return value;
}
export const hostURL = (host: string, suffix = "") =>
  "/api/v1/hosts/" + segment(host) + suffix;
export const engineURL = (kind: string, id?: string, action?: string) =>
  "/api/v1/engine/" +
  kind +
  (id === undefined ? "" : "/" + segment(id)) +
  (action ? "/" + action : "");
