import { getEarnProductAssets } from "../earn-product-mints";

describe("getEarnProductAssets", () => {
  afterEach(() => {
    jest.restoreAllMocks();
  });

  it("warns when localnet falls back to the mainnet catalog", () => {
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});

    getEarnProductAssets("localnet");

    expect(warn).toHaveBeenCalledWith(
      expect.stringContaining("[earn] localnet has no Earn product mints"),
    );
  });
});
