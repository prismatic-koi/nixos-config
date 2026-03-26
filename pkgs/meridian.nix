{
  lib,
  stdenv,
  fetchurl,
  nodejs_22,
  makeWrapper,
}:

let
  claudeAgentSdk = fetchurl {
    url = "https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.2.84.tgz";
    hash = "sha256-j1PCt8ZxVT+hYfrUc8VP12pmefhdxd9j54lcM9ATEDw=";
  };
in
stdenv.mkDerivation rec {
  pname = "meridian";
  version = "1.18.0";

  src = fetchurl {
    url = "https://registry.npmjs.org/opencode-claude-max-proxy/-/opencode-claude-max-proxy-${version}.tgz";
    hash = "sha256-UqYwANqqJVnXVpl8SKDFdZodqKxAnwukI2gRIpWrSjQ=";
  };

  nativeBuildInputs = [ makeWrapper ];
  buildInputs = [ nodejs_22 ];

  dontBuild = true;

  unpackPhase = ''
    # unpack meridian
    tar -xzf $src
    mv package meridian

    # unpack @anthropic-ai/claude-agent-sdk into node_modules
    mkdir -p meridian/node_modules/@anthropic-ai
    tar -xzf ${claudeAgentSdk}
    mv package meridian/node_modules/@anthropic-ai/claude-agent-sdk
  '';

  installPhase = ''
    mkdir -p $out/lib/meridian $out/bin
    cp -r meridian/dist meridian/package.json meridian/node_modules $out/lib/meridian/

    makeWrapper ${nodejs_22}/bin/node $out/bin/meridian \
      --add-flags "$out/lib/meridian/dist/cli.js"

    makeWrapper ${nodejs_22}/bin/node $out/bin/claude-max-proxy \
      --add-flags "$out/lib/meridian/dist/cli.js"
  '';

  meta = {
    description = "Local Anthropic API proxy powered by your Claude Max subscription";
    homepage = "https://github.com/rynfar/opencode-claude-max-proxy";
    license = lib.licenses.mit;
    maintainers = [ ];
    mainProgram = "meridian";
    # claude-agent-sdk bundles platform-specific binaries; mark platforms accordingly
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
      "x86_64-darwin"
      "aarch64-darwin"
    ];
  };
}
