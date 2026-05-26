import { useEffect, useState } from "react";
import { KeyboardAvoidingView, Linking, Platform, View } from "react-native";
import { SafeAreaView } from "react-native-safe-area-context";
import { router } from "expo-router";
import * as Haptics from "expo-haptics";
import { Text } from "@/components/ui/text";
import { TextField } from "@/components/ui/text-field";
import { Button } from "@/components/ui/button";
import { MulticaLogo } from "@/components/brand/multica-logo";
import { api } from "@/data/api";
import { useAuthStore } from "@/data/auth-store";
import { clearConsumedHandoffToken } from "@/data/secure-storage";
import { mapAuthError } from "@/lib/auth-error";

const WEB_URL = process.env.EXPO_PUBLIC_WEB_URL?.replace(/\/$/, "") ?? "";

export default function Login() {
  const sendCode = useAuthStore((s) => s.sendCode);
  const [email, setEmail] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [checkingConfig, setCheckingConfig] = useState(true);
  const [larkAuthEnabled, setLarkAuthEnabled] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    let cancelled = false;
    void api
      .getConfig({ signal: controller.signal })
      .then((config) => {
        if (cancelled) return;
        setLarkAuthEnabled(config.lark_auth_enabled === true);
      })
      .catch(() => {
        if (cancelled) return;
        setLarkAuthEnabled(false);
      })
      .finally(() => {
        if (cancelled) return;
        setCheckingConfig(false);
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, []);

  const showLarkLogin = larkAuthEnabled && WEB_URL.length > 0;
  const showEmailLogin = !checkingConfig && !showLarkLogin;

  const onSubmit = async () => {
    const trimmed = email.trim();
    if (!trimmed) return;
    void Haptics.selectionAsync();
    setSubmitting(true);
    setError(null);
    try {
      await sendCode(trimmed);
      router.push({ pathname: "/verify", params: { email: trimmed } });
    } catch (err) {
      void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
      setError(mapAuthError(err, "Couldn't send the code. Try again."));
    } finally {
      setSubmitting(false);
    }
  };

  const onLarkLogin = async () => {
    if (!WEB_URL) {
      setError("Lark login is not configured for this build.");
      return;
    }
    void Haptics.selectionAsync();
    setSubmitting(true);
    setError(null);
    try {
      await clearConsumedHandoffToken();
      await Linking.openURL(`${WEB_URL}/login?platform=mobile`);
    } catch (err) {
      void Haptics.notificationAsync(Haptics.NotificationFeedbackType.Error);
      setError(mapAuthError(err, "Couldn't open Lark login. Try again."));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <SafeAreaView className="flex-1 bg-background">
      <KeyboardAvoidingView
        className="flex-1"
        behavior={Platform.OS === "ios" ? "padding" : undefined}
      >
        <View className="flex-1 justify-center px-6 gap-6">
          <View className="items-center gap-3">
            <MulticaLogo size={32} />
            <View className="gap-1 items-center">
              <Text className="text-2xl font-semibold text-foreground">
                Sign in to Multica
              </Text>
              <Text className="text-sm text-muted-foreground text-center">
                {checkingConfig
                  ? "Checking sign-in options..."
                  : showLarkLogin
                    ? "Use Lark one-click sign-in to continue."
                    : "Enter your email and we'll send you a verification code."}
              </Text>
            </View>
          </View>

          {showEmailLogin ? (
            <>
              <View className="gap-3">
                <TextField
                  autoCapitalize="none"
                  autoComplete="email"
                  autoFocus
                  keyboardType="email-address"
                  placeholder="you@example.com"
                  value={email}
                  onChangeText={setEmail}
                  onSubmitEditing={onSubmit}
                  returnKeyType="send"
                  editable={!submitting}
                  invalid={!!error}
                />
                {error ? (
                  <Text className="text-sm text-destructive">{error}</Text>
                ) : null}
              </View>

              <Button
                size="lg"
                disabled={submitting || !email.trim()}
                onPress={onSubmit}
              >
                <Text>{submitting ? "Sending..." : "Send code"}</Text>
              </Button>
            </>
          ) : (
            <View className="gap-3">
              {error ? (
                <Text className="text-sm text-destructive text-center">
                  {error}
                </Text>
              ) : null}
              <Button
                size="lg"
                disabled={checkingConfig || submitting}
                onPress={onLarkLogin}
              >
                <Text>
                  {checkingConfig
                    ? "Checking..."
                    : submitting
                      ? "Opening..."
                      : "Lark one-click sign-in"}
                </Text>
              </Button>
            </View>
          )}
        </View>
      </KeyboardAvoidingView>
    </SafeAreaView>
  );
}
