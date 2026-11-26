import * as Haptics from "expo-haptics";

import { withTapHaptic } from "@/lib/haptics";

jest.mock("react-native", () => ({ Platform: { OS: "android" } }));
jest.mock("expo-haptics", () => ({
  AndroidHaptics: { Virtual_Key: "virtual-key" },
  performAndroidHapticsAsync: jest.fn(),
}));

it("runs a tap immediately and once even if Android feedback fails", async () => {
  jest.mocked(Haptics.performAndroidHapticsAsync).mockRejectedValueOnce(
    new Error("Feedback unavailable"),
  );
  const navigate = jest.fn();
  const press = withTapHaptic(navigate);
  expect(Haptics.performAndroidHapticsAsync).not.toHaveBeenCalled();

  press();
  expect(navigate).toHaveBeenCalledTimes(1);
  expect(Haptics.performAndroidHapticsAsync).toHaveBeenCalledTimes(1);
  await Promise.resolve();
  expect(navigate).toHaveBeenCalledTimes(1);
});
