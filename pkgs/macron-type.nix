{
  lib,
  swiftPackages,
}:

swiftPackages.stdenv.mkDerivation {
  pname = "macron-type";
  version = "0.1.0";

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
    description = "Post a Unicode character via CGEventPost without Accessibility permission";
    mainProgram = "macron-type";
    platforms = lib.platforms.darwin;
    license = lib.licenses.mit;
  };
}
