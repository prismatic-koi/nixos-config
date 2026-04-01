{
  lib,
  stdenv,
  swiftPackages,
}:

let
  # Socket server: runs as LaunchAgent in user session, posts CGEvents
  server = swiftPackages.stdenv.mkDerivation {
    pname = "macron-type";
    version = "0.2.0";

    src = ../modules/programs/macron-type;

    nativeBuildInputs = [ swiftPackages.swift ];

    buildPhase = ''
      runHook preBuild
      swiftc main.swift -o macron-type \
        -framework CoreGraphics \
        -framework Foundation
      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall
      mkdir -p $out/bin
      cp macron-type $out/bin/
      runHook postInstall
    '';

    meta = {
      description = "Socket server that posts Unicode macron vowels via CGEventPost";
      mainProgram = "macron-type";
      platforms = lib.platforms.darwin;
      license = lib.licenses.mit;
    };
  };

  # Tiny client: called by Karabiner shell_command, sends one byte to the server
  client = stdenv.mkDerivation {
    pname = "macron-send";
    version = "0.2.0";

    src = ../modules/programs/macron-type;

    buildPhase = ''
      runHook preBuild
      cc -o macron-send macron-send.c
      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall
      mkdir -p $out/bin
      cp macron-send $out/bin/
      runHook postInstall
    '';

    meta = {
      description = "Client for macron-type socket server";
      mainProgram = "macron-send";
      platforms = lib.platforms.darwin;
      license = lib.licenses.mit;
    };
  };
in
{
  inherit server client;
}
