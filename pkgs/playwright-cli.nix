{
  lib,
  stdenv,
  buildNpmPackage,
  fetchFromGitHub,
  makeWrapper,
  writeShellScript,
  chromium,
  playwright-driver,
}:

let
  isDarwin = stdenv.hostPlatform.isDarwin;

  # On Darwin we cannot hard-code the chromium executable path because the
  # playwright-driver browsers derivation embeds a chromium revision string
  # (e.g. `chromium-1200`), an architecture-dependent dir name
  # (`chrome-mac-arm64` vs `chrome-mac`), and a binary name that upstream
  # Google have renamed before (currently "Google Chrome for Testing"). A
  # runtime glob resolves the binary from the stable browsers-root path,
  # which makes the wrapper resilient to all three moving parts.
  browsersPath = "${playwright-driver.browsers-chromium}";

  # Shell launcher that resolves the chromium executable at exec time and
  # exec's it. PLAYWRIGHT_MCP_EXECUTABLE_PATH on Darwin points at this
  # launcher so the resolution happens lazily, not at build time.
  darwinChromiumLauncher = writeShellScript "playwright-cli-browser-launcher" ''
    set -eu
    browsers_path=${lib.escapeShellArg browsersPath}
    # The browsers derivation contains:
    #   <browsers_path>/chromium-<rev>/chrome-mac{,-arm64}/<App>.app/Contents/MacOS/<binary>
    # All three of <rev>, the arch dir, and the binary name have shifted
    # upstream before; resolve them by glob and take the first match.
    exe=""
    for candidate in "$browsers_path"/chromium-*/chrome-mac*/*.app/Contents/MacOS/*; do
      if [ -x "$candidate" ]; then
        exe="$candidate"
        break
      fi
    done
    if [ -z "$exe" ]; then
      echo "playwright-cli: could not locate chromium binary under $browsers_path" >&2
      exit 1
    fi
    exec "$exe" "$@"
  '';

  chromiumExecutablePath =
    if isDarwin then "${darwinChromiumLauncher}" else "${chromium}/bin/chromium";
in
buildNpmPackage rec {
  pname = "playwright-cli";
  version = "0.1.13";

  src = fetchFromGitHub {
    owner = "microsoft";
    repo = "playwright-cli";
    rev = "v${version}";
    hash = "sha256-hHK/GR5Drlt+e0L9kyNmn+ht1PCrVH6WrVbxGB1Wsxg=";
  };

  npmDepsHash = "sha256-Ulp6IttsZcOOA7LaYDpVKkBYbe2j4RFG8lJARWifOSk=";

  nativeBuildInputs = [ makeWrapper ];

  dontNpmBuild = true;

  postFixup = ''
    wrapProgram $out/bin/playwright-cli \
      --set-default PLAYWRIGHT_MCP_EXECUTABLE_PATH ${chromiumExecutablePath} \
      --set-default PLAYWRIGHT_MCP_BROWSER chromium \
      --set-default PLAYWRIGHT_MCP_HEADLESS false
  '';

  meta = {
    description = "Playwright CLI with skills for browser automation in coding agents";
    homepage = "https://github.com/microsoft/playwright-cli";
    license = lib.licenses.asl20;
    maintainers = [ ];
    mainProgram = "playwright-cli";
  };
}
