import type { Workspace } from "../types";
import { useAuthStore } from "../auth";
import { paths } from "./paths";

/**
 * Priority (workspace-first):
 *   workspace[0] → /<first.slug>/issues
 *   no workspace → /workspaces/new
 *
 * Self-host/local deployments intentionally send first-time users without
 * invitations to workspace creation after login. `hasOnboarded` is accepted
 * for API compatibility with callers but is not a routing gate here.
 *
 * Callers that need invitation-aware routing (callback / login) handle
 * the "un-onboarded with pending invites" branch themselves before calling
 * this resolver — this resolver only deals with the post-invite-check
 * destination.
 */
export function resolvePostAuthDestination(
  workspaces: Workspace[],
  _hasOnboarded: boolean,
): string {
  const first = workspaces[0];
  if (first) {
    return paths.workspace(first.slug).issues();
  }
  return paths.newWorkspace();
}

/**
 * Single source of truth: backed by `users.onboarded_at`, which
 * arrives with the user object on every auth response.
 */
export function useHasOnboarded(): boolean {
  return useAuthStore((s) => s.user?.onboarded_at != null);
}
