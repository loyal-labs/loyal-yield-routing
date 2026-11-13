/**
 * Lazily load expo-notifications to avoid crash in Expo Go (SDK 53+).
 * Returns null if the module can't be loaded.
 */
async function getNotificationsModule() {
  try {
    return await import("expo-notifications");
  } catch {
    console.log("expo-notifications not available (Expo Go?)");
    return null;
  }
}

/**
 * Configure notification display behavior. Call once on app boot.
 *
 * Also installs a default Android channel — Android 8+ drops notifications
 * silently if the sender doesn't target a registered channel, so we always
 * need at least one even for low-volume delivery.
 */
export async function setupNotificationHandler(): Promise<void> {
  const Notifications = await getNotificationsModule();
  if (!Notifications) return;

  Notifications.setNotificationHandler({
    handleNotification: async () => ({
      shouldShowAlert: true,
      shouldShowBanner: true,
      shouldShowList: true,
      shouldPlaySound: true,
      shouldSetBadge: true,
    }),
  });

  if (process.env.EXPO_OS === "android") {
    await Notifications.setNotificationChannelAsync("default", {
      name: "Default",
      importance: Notifications.AndroidImportance.HIGH,
      lockscreenVisibility: Notifications.AndroidNotificationVisibility.PUBLIC,
      sound: "default",
      vibrationPattern: [0, 250, 250, 250],
    });
  }
}

export async function addNotificationResponseListener(
  callback: (data: Record<string, unknown>) => void,
): Promise<(() => void) | null> {
  const Notifications = await getNotificationsModule();
  if (!Notifications) return null;

  const subscription =
    Notifications.addNotificationResponseReceivedListener((response) => {
      const data = response.notification.request.content.data;
      if (data) callback(data);
    });

  return () => subscription.remove();
}
