import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import {
  CLI_INSTALL_COMMANDS,
  CliInstallCommand,
} from "./cli-install-command";

const LABELS = {
  group: "Choose your platform",
  macosLinux: "macOS / Linux",
  windows: "Windows",
};

function renderSwitch() {
  return render(
    <CliInstallCommand labels={LABELS}>
      {(command) => <code>{command}</code>}
    </CliInstallCommand>,
  );
}

describe("CliInstallCommand", () => {
  it("keeps both platform installers on the localized release repository", () => {
    expect(CLI_INSTALL_COMMANDS.macosLinux).toBe(
      "curl -fsSL https://raw.githubusercontent.com/kanfashidoufu/multica/main/scripts/install.sh | bash",
    );
    expect(CLI_INSTALL_COMMANDS.windows).toBe(
      "irm https://raw.githubusercontent.com/kanfashidoufu/multica/main/scripts/install.ps1 | iex",
    );
  });

  it("defaults to the macOS / Linux installer", () => {
    renderSwitch();

    expect(
      screen.getByRole("tablist", { name: LABELS.group }),
    ).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "macOS / Linux" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(
      screen.getByText(CLI_INSTALL_COMMANDS.macosLinux),
    ).toBeInTheDocument();
    expect(screen.queryByText(CLI_INSTALL_COMMANDS.windows)).toBeNull();
  });

  it("hands the Windows command to the row when that tab is picked", async () => {
    const user = userEvent.setup();
    renderSwitch();

    await user.click(screen.getByRole("tab", { name: "Windows" }));

    expect(screen.getByRole("tab", { name: "Windows" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(screen.getByText(CLI_INSTALL_COMMANDS.windows)).toBeInTheDocument();
    expect(screen.queryByText(CLI_INSTALL_COMMANDS.macosLinux)).toBeNull();
  });

  // A real tablist: arrow keys move between platforms without a click.
  it("switches platform with the arrow keys", async () => {
    const user = userEvent.setup();
    renderSwitch();

    await user.click(screen.getByRole("tab", { name: "macOS / Linux" }));
    await user.keyboard("{ArrowRight}");

    expect(screen.getByText(CLI_INSTALL_COMMANDS.windows)).toBeInTheDocument();
  });
});
