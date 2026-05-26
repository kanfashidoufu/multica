import { useEffect, useRef } from "react";
import { ActivityIndicator, View } from "react-native";
import { router, useLocalSearchParams } from "expo-router";
import { useQueryClient } from "@tanstack/react-query";
import { Text } from "@/components/ui/text";
import { useAuthStore } from "@/data/auth-store";
import {
  getToken,
  isHandoffTokenConsumed,
  markHandoffTokenConsumed,
} from "@/data/secure-storage";

export default function AuthCallback() {
  const qc = useQueryClient();
  const loginWithToken = useAuthStore((s) => s.loginWithToken);
  const { token } = useLocalSearchParams<{ token?: string | string[] }>();
  const consumedRef = useRef(false);

  useEffect(() => {
    if (consumedRef.current) return;
    consumedRef.current = true;

    const resolvedToken = Array.isArray(token) ? token[0] : token;
    if (!resolvedToken) {
      router.replace("/login");
      return;
    }

    void (async () => {
      try {
        if (await isHandoffTokenConsumed(resolvedToken)) {
          const existingToken = await getToken();
          router.replace(existingToken ? "/" : "/login");
          return;
        }
        qc.clear();
        await loginWithToken(resolvedToken);
        await markHandoffTokenConsumed(resolvedToken);
        router.replace("/");
      } catch (err) {
        console.warn("[auth] failed to complete deep-link login", err);
        router.replace("/login");
      }
    })();
  }, [loginWithToken, qc, token]);

  return (
    <View className="flex-1 items-center justify-center gap-3 bg-background px-6">
      <ActivityIndicator />
      <Text className="text-sm text-muted-foreground">
        Completing sign-in...
      </Text>
    </View>
  );
}
