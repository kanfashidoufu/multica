import { createStore } from "zustand/vanilla";
import { useStore } from "zustand";

interface ConfigState {
  cdnDomain: string;
  allowSignup: boolean;
  googleClientId: string;
  larkAuthEnabled: boolean;
  larkAppId: string;
  larkAuthorizeUrl: string;
  releaseRepository: string;
  // Self-host gate (#3433): when true, every "Create workspace" affordance
  // must be hidden. Defaults to false so unknown / older servers behave like
  // the managed-cloud case.
  workspaceCreationDisabled: boolean;
  setCdnDomain: (domain: string) => void;
  setAuthConfig: (config: {
    allowSignup: boolean;
    googleClientId?: string;
    larkAuthEnabled?: boolean;
    larkAppId?: string;
    larkAuthorizeUrl?: string;
    releaseRepository?: string;
    workspaceCreationDisabled?: boolean;
  }) => void;
}

export const configStore = createStore<ConfigState>((set) => ({
  cdnDomain: "",
  allowSignup: true,
  googleClientId: "",
  larkAuthEnabled: false,
  larkAppId: "",
  larkAuthorizeUrl: "",
  releaseRepository: "",
  workspaceCreationDisabled: false,
  setCdnDomain: (domain) => set({ cdnDomain: domain }),
  setAuthConfig: ({
    allowSignup,
    googleClientId = "",
    larkAuthEnabled = false,
    larkAppId = "",
    larkAuthorizeUrl = "",
    releaseRepository = "",
    workspaceCreationDisabled = false,
  }) =>
    set({
      allowSignup,
      googleClientId,
      larkAuthEnabled,
      larkAppId,
      larkAuthorizeUrl,
      releaseRepository,
      workspaceCreationDisabled,
    }),
}));

export function useConfigStore(): ConfigState;
export function useConfigStore<T>(selector: (state: ConfigState) => T): T;
export function useConfigStore<T>(selector?: (state: ConfigState) => T) {
  return useStore(configStore, selector as (state: ConfigState) => T);
}
