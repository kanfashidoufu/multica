import { describe, expect, it } from "vitest";
import type { Workspace } from "../types";
import { paths } from "./paths";
import { resolvePostAuthDestination } from "./resolve";

function makeWs(slug: string): Workspace {
  return {
    id: `id-${slug}`,
    name: slug,
    slug,
    description: null,
    context: null,
    settings: {},
    repos: [],
    issue_prefix: slug.toUpperCase(),
    avatar_url: null,
    created_at: "",
    updated_at: "",
  };
}

describe("resolvePostAuthDestination", () => {
  it("!onboarded + workspace[0] → /<first.slug>/issues", () => {
    const ws = [makeWs("acme")];
    expect(resolvePostAuthDestination(ws, false)).toBe(
      paths.workspace("acme").issues(),
    );
  });

  it("onboarded + workspace[0] → /<first.slug>/issues", () => {
    const ws = [makeWs("acme"), makeWs("beta")];
    expect(resolvePostAuthDestination(ws, true)).toBe(
      paths.workspace("acme").issues(),
    );
  });

  it("onboarded + no workspace → /workspaces/new", () => {
    expect(resolvePostAuthDestination([], true)).toBe(paths.newWorkspace());
  });

  it("!onboarded + no workspace → /workspaces/new", () => {
    // Self-host/local deployments use workspace creation as the post-login
    // first-run path when there are no pending invitations.
    expect(resolvePostAuthDestination([], false)).toBe(paths.newWorkspace());
  });
});
