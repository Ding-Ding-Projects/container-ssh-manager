import { defineConfig } from "vitest/config";
export default defineConfig({
  test: { environment: "happy-dom", maxWorkers: 2, fileParallelism: false },
});
