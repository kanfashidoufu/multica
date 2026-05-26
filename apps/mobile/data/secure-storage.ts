/**
 * Thin wrapper around expo-secure-store for the auth token.
 * Keyed identically to web/desktop ("multica_token") so logic stays aligned
 * with packages/core/auth/store.ts even though storage backends differ.
 */
import * as SecureStore from "expo-secure-store";

const TOKEN_KEY = "multica_token";
const CONSUMED_HANDOFF_TOKEN_KEY = "multica_consumed_handoff_token";

export async function getToken(): Promise<string | null> {
  return SecureStore.getItemAsync(TOKEN_KEY);
}

export async function setToken(token: string): Promise<void> {
  await SecureStore.setItemAsync(TOKEN_KEY, token);
}

export async function clearToken(): Promise<void> {
  await SecureStore.deleteItemAsync(TOKEN_KEY);
}

function tokenFingerprint(token: string): string {
  let hash = 2166136261;
  for (let i = 0; i < token.length; i += 1) {
    hash ^= token.charCodeAt(i);
    hash = Math.imul(hash, 16777619);
  }
  return hash.toString(16);
}

export async function markHandoffTokenConsumed(token: string): Promise<void> {
  await SecureStore.setItemAsync(
    CONSUMED_HANDOFF_TOKEN_KEY,
    tokenFingerprint(token),
  );
}

export async function clearConsumedHandoffToken(): Promise<void> {
  await SecureStore.deleteItemAsync(CONSUMED_HANDOFF_TOKEN_KEY);
}

export async function isHandoffTokenConsumed(token: string): Promise<boolean> {
  const consumed = await SecureStore.getItemAsync(CONSUMED_HANDOFF_TOKEN_KEY);
  return consumed === tokenFingerprint(token);
}
