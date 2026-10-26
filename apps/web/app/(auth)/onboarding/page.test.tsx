import { beforeEach, describe, expect, it, vi } from "vitest";
import { render } from "@testing-library/react";
import OnboardingPage from "./page";

const { router, authState, workspaceList } = vi.hoisted(() => ({
  router: { replace: vi.fn() },
  authState: {
    user: { id: "local-user", onboarded_at: null } as {
      id: string;
      onboarded_at: string | null;
    } | null,
    isLoading: false,
  },
  workspaceList: { workspaces: [] as { slug: string }[], ready: true },
}));

vi.mock("next/navigation", () => ({ useRouter: () => router }));
vi.mock("@multica/core/auth", () => ({
  useAuthStore: Object.assign(
    (selector: (state: typeof authState) => unknown) => selector(authState),
    { getState: () => authState },
  ),
}));
vi.mock("@multica/core/workspace", () => ({
  useWorkspaceList: () => workspaceList,
}));

describe("localized onboarding route", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    authState.user = { id: "local-user", onboarded_at: null };
    authState.isLoading = false;
    workspaceList.workspaces = [];
    workspaceList.ready = true;
  });

  it("sends a first-time user directly to workspace creation", () => {
    const { container } = render(<OnboardingPage />);

    expect(router.replace).toHaveBeenCalledWith("/workspaces/new");
    expect(container).toBeEmptyDOMElement();
  });

  it("opens an existing workspace without requiring the questionnaire", () => {
    workspaceList.workspaces = [{ slug: "local-team" }];

    render(<OnboardingPage />);

    expect(router.replace).toHaveBeenCalledWith("/local-team/issues");
  });

  it("waits for auth and workspace loading before choosing a destination", () => {
    authState.isLoading = true;
    const { rerender } = render(<OnboardingPage />);
    expect(router.replace).not.toHaveBeenCalled();

    authState.isLoading = false;
    workspaceList.ready = false;
    rerender(<OnboardingPage />);
    expect(router.replace).not.toHaveBeenCalled();

    workspaceList.ready = true;
    rerender(<OnboardingPage />);
    expect(router.replace).toHaveBeenCalledWith("/workspaces/new");
  });

  it("redirects an unauthenticated user to login", () => {
    authState.user = null;

    render(<OnboardingPage />);

    expect(router.replace).toHaveBeenCalledWith("/login");
  });
});
