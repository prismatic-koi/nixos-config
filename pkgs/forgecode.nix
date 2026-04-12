{
  lib,
  stdenv,
  fetchurl,
  autoPatchelfHook,
  libgcc,
}:

let
  version = "2.9.9";

  sources = {
    "x86_64-linux" = {
      url = "https://github.com/antinomyhq/forgecode/releases/download/v${version}/forge-x86_64-unknown-linux-gnu";
      hash = "sha256-P95V3bXqOlqGpxCDzZ2q7yIIwqecSktTrPaSjXWDIhk=";
    };
    "aarch64-darwin" = {
      url = "https://github.com/antinomyhq/forgecode/releases/download/v${version}/forge-aarch64-apple-darwin";
      hash = "sha256-KeAZTzLdrjKvOWjJl1EKp6vLgUKhYhtdSPQbgB6izsU=";
    };
  };

  source =
    sources.${stdenv.hostPlatform.system}
      or (throw "forgecode: unsupported platform ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation {
  pname = "forgecode";
  inherit version;

  src = fetchurl {
    inherit (source) url hash;
  };

  nativeBuildInputs = lib.optionals stdenv.hostPlatform.isLinux [ autoPatchelfHook ];

  buildInputs = lib.optionals stdenv.hostPlatform.isLinux [ libgcc ];

  dontUnpack = true;
  dontBuild = true;

  installPhase = ''
    install -Dm755 $src $out/bin/forge
  '';

  meta = {
    description = "AI-enabled pair programmer for Claude, GPT, and 300+ models";
    homepage = "https://forgecode.dev";
    license = lib.licenses.asl20;
    mainProgram = "forge";
    platforms = [
      "x86_64-linux"
      "aarch64-darwin"
    ];
  };
}
