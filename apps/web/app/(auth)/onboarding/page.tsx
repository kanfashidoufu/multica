"use client";

import { useEffect, useRef } from "react";
import { useRouter } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useAuthStore } from "@multica/core/auth";
import {
  paths,
  resolvePostAuthDestination,
  useHasOnboarded,
} from "@multica/core/paths";
import { workspaceListOptions } from "@multica/core/workspace/queries";

/**
 * Web shell for the onboarding flow. The route is the platform chrome on
 * web (matching `WindowOverlay` on desktop); content is the shared
 * `<OnboardingFlow />`. Kept minimal — guard on auth, render, exit.
 *
 * On complete: runtime-connected onboarding may provide a guide issue id;
 * navigate there. Otherwise land on the workspace issues list, or root if
 * the flow never produced a workspace.
 *
 * `CliInstallInstructions` is passed in as the `runtimeInstructions`
 * slot so the flow can render it inside the CLI dialog. The commands it
 * shows are hardcoded — nothing environmental to thread through.
 * Legacy onboarding route. First-run onboarding is bypassed, so this route
 * immediately hands users to the standard post-auth destination.
 */
export default function OnboardingPage() {
  const router = useRouter();
  const user = useAuthStore((s) => s.user);
  const isLoading = useAuthStore((s) => s.isLoading);
  const hasOnboarded = useHasOnboarded();
  const { data: workspaces = [], isFetched: workspacesFetched } = useQuery({
    ...workspaceListOptions(),
    enabled: !!user,
  });
  // The bootstrap path calls refreshMe() before returning, which flips
  // hasOnboarded to true while the page is still mounted. Without this
  // flag the guard below races onComplete: the guard's router.replace
  // (issues list) can overtake onComplete's router.push (guide issue),
  // dropping the user on the wrong destination. Marking the page as
  // "completing" right before onComplete navigates keeps the guard
  // silent for the in-flight transition.
  const completingRef = useRef(false);

  useEffect(() => {
    if (isLoading || !user) {
      if (!isLoading && !user) router.replace(paths.login());
      return;
    }
    if (!workspacesFetched) return;
    router.replace(resolvePostAuthDestination(workspaces, hasOnboarded));
  }, [isLoading, user, hasOnboarded, workspacesFetched, workspaces, router]);

  return null;
}
