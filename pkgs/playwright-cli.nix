{
  lib,
  buildNpmPackage,
  fetchFromGitHub,
  makeWrapper,
  chromium,
}:

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
      --set-default PLAYWRIGHT_MCP_EXECUTABLE_PATH ${chromium}/bin/chromium \
      --set-default PLAYWRIGHT_MCP_BROWSER chromium \
      --set-default PLAYWRIGHT_MCP_HEADLESS true
  '';

  meta = {
    description = "Playwright CLI with skills for browser automation in coding agents";
    homepage = "https://github.com/microsoft/playwright-cli";
    license = lib.licenses.asl20;
    maintainers = [ ];
    mainProgram = "playwright-cli";
  };
}
