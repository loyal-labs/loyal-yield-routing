import { notificationRoute } from "../notification-route";

describe("notificationRoute", () => {
  it("accepts only the two earnings routes", () => {
    expect(notificationRoute({ route: "/(tabs)/earn" })).toBe("/(tabs)/earn");
    expect(notificationRoute({ route: "/(tabs)/earn-max" })).toBe(
      "/(tabs)/earn-max",
    );
  });

  it("ignores anything else", () => {
    for (const data of [
      undefined,
      null,
      {},
      { route: "/settings" },
      { route: "https://evil.example" },
      { route: 42 },
      "x",
    ]) {
      expect(notificationRoute(data)).toBeNull();
    }
  });
});
