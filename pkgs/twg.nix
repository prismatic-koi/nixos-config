{
  lib,
  stdenv,
  fetchurl,
  patchelf,
}:

let
  version = "1.4.4";
  # SRI form of the hashes in
  # https://teamwork-graph.atlassian.com/cli/SHA256SUMS-v1.4.4
  sources = {
    aarch64-darwin = {
      platform = "darwin-arm64";
      hash = "sha256-o0Jw7gesinAdurP4vmaaxkDldf3IV1jVxWhGl1p9zuw=";
    };
    x86_64-darwin = {
      platform = "darwin-x64";
      hash = "sha256-xggXMIhqTJIDd/oXwOLWdPpQ7GpLTTeev7LveMRUvr0=";
    };
    x86_64-linux = {
      platform = "linux-x64";
      hash = "sha256-Nqk7CtABHYCQnMAFCAkcNN8D3QvxZE/e1nv5Tyyf/Ac=";
    };
    aarch64-linux = {
      platform = "linux-arm64";
      hash = "sha256-TattT7yU5FD8eeZhrEhha6EJm0AHfe3FUNTRBeHLg8w=";
    };
  };
  source =
    sources.${stdenv.hostPlatform.system}
      or (throw "twg: unsupported system ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation {
  pname = "twg";
  inherit version;

  src = fetchurl {
    url = "https://teamwork-graph.atlassian.com/cli/twg-${source.platform}-v${version}";
    inherit (source) hash;
  };

  dontUnpack = true;
  dontConfigure = true;
  dontBuild = true;

  # twg is a Bun single-file executable: the JavaScript payload is part of
  # the file. Strip can delete the payload. On Darwin, any change to the
  # file also breaks the code signature.
  dontStrip = true;
  # The patchELF fixup hook rewrites every ELF file. installPhase sets the
  # interpreter explicitly, and no other change to the file is necessary.
  dontPatchELF = true;

  nativeBuildInputs = lib.optionals stdenv.hostPlatform.isLinux [ patchelf ];

  # Linux method: interpreter patch, not buildFHSEnv. The upstream binary
  # asks for /lib64/ld-linux-*.so.*, which NixOS does not supply. Its only
  # NEEDED libraries are glibc libraries, which the Nix glibc loader finds
  # by itself, so no RPATH is necessary. The Bun payload is in the `.bun`
  # ELF section, inside a PT_LOAD segment. `patchelf --set-interpreter`
  # moves the file offsets but keeps every loaded segment at the same
  # virtual address with the same bytes, so the payload stays intact.
  installPhase = ''
    runHook preInstall
    install -Dm755 "$src" "$out/bin/twg"
  ''
  + lib.optionalString stdenv.hostPlatform.isLinux ''
    patchelf --set-interpreter "$(cat "$NIX_CC/nix-support/dynamic-linker")" "$out/bin/twg"
  ''
  + ''
    runHook postInstall
  '';

  meta = {
    description = "Atlassian Teamwork Graph CLI";
    homepage = "https://teamworkgraph.com/cli";
    license = lib.licenses.unfree;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    platforms = builtins.attrNames sources;
    mainProgram = "twg";
  };
}
