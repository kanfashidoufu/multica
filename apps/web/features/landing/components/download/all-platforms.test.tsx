import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { createEnDict } from "../../i18n/en";
import { AllPlatforms } from "./all-platforms";

vi.mock("next/navigation", () => ({ useRouter: () => ({ push: vi.fn() }) }));
vi.mock("../../i18n", () => ({ useLocale: () => ({ t: { download: createEnDict().download } }) }));

const ASSETS = {
  macArm64Dmg: "https://downloads.test/mac-arm64.dmg",
  macArm64Zip: "https://downloads.test/mac-arm64.zip",
  macX64Dmg: "https://downloads.test/mac-x64.dmg",
  macX64Zip: "https://downloads.test/mac-x64.zip",
};
const ALL_RELEASES_URL = "https://github.com/example/private-multica/releases";

function row(arch: string) {
  return screen.getByText(arch).closest("li")!;
}

describe("AllPlatforms", () => {
  it("lists Intel macOS downloads separately from Apple Silicon", () => {
    render(<AllPlatforms assets={ASSETS} allReleasesUrl={ALL_RELEASES_URL} />);

    const intel = within(row("Intel"));
    expect(intel.getByRole("link", { name: ".dmg" })).toHaveAttribute("href", ASSETS.macX64Dmg);
    expect(intel.getByRole("link", { name: ".zip" })).toHaveAttribute("href", ASSETS.macX64Zip);
    expect(screen.getByRole("link", { name: "All releases" })).toHaveAttribute("href", ALL_RELEASES_URL);
  });

  it("marks the visitor's platform and disables formats the release lacks", () => {
    render(<AllPlatforms assets={ASSETS} allReleasesUrl={ALL_RELEASES_URL} current="macArm64Dmg" />);

    expect(within(row("Apple Silicon")).getByText("This computer")).toBeInTheDocument();
    expect(within(row("Intel")).queryByText("This computer")).toBeNull();
    const windows = screen.getAllByText("Windows")[0]!.closest("li")!;
    expect(within(windows).queryByRole("link")).toBeNull();
  });
});
