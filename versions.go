package main

import (
	"fmt"
	"os"
)

// versionsScript prints the version of every tool that is installed in the
// image; it only reads, so it is safe to run in a throwaway container.
const versionsScript = `
v() { command -v "$1" >/dev/null 2>&1 && printf '%-12s %s\n' "$1" "$("$@" 2>&1 | head -n1)"; }
v git --version
v node --version
v npm --version
v copilot --version
v java -version
v go version
v rustc --version
v cargo --version
v gcc --version
v g++ --version
v cmake --version
v ninja --version
v vim --version
v less --version
if [ -d "${PLAYWRIGHT_BROWSERS_PATH:-/ms-playwright}" ]; then
  printf '%-12s %s\n' playwright-browsers "$(ls "${PLAYWRIGHT_BROWSERS_PATH:-/ms-playwright}" | tr '\n' ' ')"
fi
`

// cmdVersions reports the tool versions that the built image resolved.
func cmdVersions(args []string) error {
	cfg, _, err := parse("versions", args, nil)
	if err != nil {
		return err
	}
	if containsString(cfg.Toolchains, playwrightToolchain) {
		fmt.Printf("configured Playwright version: %s (%s)\n", cfg.PlaywrightVersion, playwrightBaseImage(cfg.PlaywrightVersion))
	}
	fmt.Fprintf(os.Stderr, "tools in image %s:\n", cfg.Image)
	return run(cfg.Runtime, "run", "--rm", "--entrypoint", "sh", cfg.Image, "-c", versionsScript)
}
