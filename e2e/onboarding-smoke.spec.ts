import { test, expect } from "@playwright/test";
import { TestApiClient } from "./fixtures";

// The local/self-host build intentionally sends a signed-in user with no
// workspaces straight to workspace creation. The hosted questionnaire is not
// a routing gate in this distribution.

test.use({ viewport: { width: 1440, height: 900 } });

test("onboarding — zero-workspace users land on workspace creation", async ({
  page,
}) => {
  const api = new TestApiClient();
  await api.login(`onboarding-local-${Date.now()}@localhost`, "Local Tester");
  const token = api.getToken();

  await page.addInitScript((t) => {
    localStorage.setItem("multica_token", t);
  }, token);
  await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(/\/workspaces\/new/);
  await expect(
    page.getByRole("heading", { name: /Name your workspace/i }),
  ).toBeVisible({ timeout: 15000 });

  const nameInput = page.getByRole("textbox", { name: "Workspace name" });
  const slugInput = page.getByRole("textbox", { name: "URL" });
  const createButton = page.getByRole("button", { name: "Create workspace" });
  await expect(createButton).toBeDisabled();

  await nameInput.fill(`Onboarding Workspace ${Date.now()}`);
  await expect(slugInput).toHaveValue(/^onboarding-workspace-\d+$/);
  await expect(
    page.getByRole("button", { name: /^Create Onboarding Workspace \d+$/ }),
  ).toBeEnabled();
  await expect(page.getByText("Tell us a bit about you.")).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Continue on web" })).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Skip" })).toHaveCount(0);
});

test("onboarding — an edited local workspace URL is preserved", async ({
  page,
}) => {
  const api = new TestApiClient();
  await api.login(`workspace-url-${Date.now()}@localhost`, "URL Tester");
  const token = api.getToken();

  await page.addInitScript((t) => localStorage.setItem("multica_token", t), token);
  await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(/\/workspaces\/new/);

  const nameInput = page.getByRole("textbox", { name: "Workspace name" });
  const slugInput = page.getByRole("textbox", { name: "URL" });
  await nameInput.fill("Initial Workspace");
  await expect(slugInput).toHaveValue("initial-workspace");
  await slugInput.fill("custom-local-workspace");
  await nameInput.fill("Renamed Workspace");
  await expect(slugInput).toHaveValue("custom-local-workspace");
  await expect(
    page.getByRole("button", { name: "Continue on web" }),
  ).toHaveCount(0);
  await expect(page.getByRole("button", { name: "Skip" })).toHaveCount(0);
});

test("onboarding — zh-Hans renders Chinese labels", async ({ page, context, baseURL }) => {
  await context.addCookies([
    {
      name: "multica-locale",
      value: "zh-Hans",
      url: baseURL ?? "http://localhost:3000",
    },
  ]);
  const api = new TestApiClient();
  await api.login(`zh-${Date.now()}@localhost`, "中文用户");
  const token = api.getToken();

  await page.addInitScript(
    (t) => localStorage.setItem("multica_token", t),
    token,
  );
  await page.goto("/onboarding", { waitUntil: "domcontentloaded" });
  await expect(page).toHaveURL(/\/workspaces\/new/);
  await expect(
    page.getByRole("heading", { name: /给工作区起个名字/ }),
  ).toBeVisible({ timeout: 15000 });
  await expect(
    page.getByRole("textbox", { name: "工作区名称" }),
  ).toBeVisible();
  await expect(
    page.getByRole("textbox", { name: "URL" }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "创建工作区" }),
  ).toBeVisible();
  await expect(page.getByText("简单介绍一下你自己。")).toHaveCount(0);
});
