"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";
import { useAuthStore } from "@multica/core/auth";
import {
  paths,
  resolvePostAuthDestination,
  useHasOnboarded,
} from "@multica/core/paths";
import { useWorkspaceList } from "@multica/core/workspace";

/**
 * Legacy onboarding route. First-run onboarding is bypassed, so this route
 * immediately hands users to the standard post-auth destination.
 * Zero-workspace users land on `/workspaces/new`, whose new-workspace mode
 * starts directly at workspace creation and therefore preserves the local
 * deployment's questionnaire bypass while using the current setup flow.
 */
export default function OnboardingPage() {
  const router = useRouter();
  const user = useAuthStore((s) => s.user);
  const isLoading = useAuthStore((s) => s.isLoading);
  const hasOnboarded = useHasOnboarded();
  const { workspaces, ready: workspacesReady } = useWorkspaceList({
    enabled: !!user,
  });
  useEffect(() => {
    if (isLoading || !user) {
      if (!isLoading && !user) router.replace(paths.login());
      return;
    }
    if (!workspacesReady) return;
    router.replace(resolvePostAuthDestination(workspaces, hasOnboarded));
  }, [isLoading, user, hasOnboarded, workspacesReady, workspaces, router]);

  return null;
}
