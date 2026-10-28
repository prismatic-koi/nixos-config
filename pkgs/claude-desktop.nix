{
  lib,
  stdenv,
  fetchurl,
  dpkg,
  autoPatchelfHook,
  makeWrapper,
  wrapGAppsHook3,
  alsa-lib,
  at-spi2-atk,
  at-spi2-core,
  cairo,
  cups,
  dbus,
  expat,
  gdk-pixbuf,
  glib,
  gtk3,
  libdrm,
  libgbm,
  libnotify,
  libcap_ng,
  libsecret,
  libseccomp,
  libuuid,
  libxkbcommon,
  libxcb,
  libx11,
  libxcomposite,
  libxdamage,
  libxext,
  libxfixes,
  libxrandr,
  libxtst,
  nspr,
  nss,
  pango,
  systemd,
  vulkan-loader,
  xdg-utils,
}:

let
  version = "1.18286.0";
  sources = {
    x86_64-linux = {
      arch = "amd64";
      hash = "sha256-jzFK0agKq1JxGo6qvAaq5I+zQfCt6koNcmTbXKudBTY=";
    };
  };
  source =
    sources.${stdenv.hostPlatform.system}
      or (throw "claude-desktop: unsupported system ${stdenv.hostPlatform.system}");
in
stdenv.mkDerivation {
  pname = "claude-desktop";
  inherit version;

  src = fetchurl {
    url = "https://downloads.claude.ai/claude-desktop/apt/stable/pool/main/c/claude-desktop/claude-desktop_${version}_${source.arch}.deb";
    inherit (source) hash;
  };

  nativeBuildInputs = [
    dpkg
    autoPatchelfHook
    makeWrapper
    wrapGAppsHook3
  ];

  buildInputs = [
    alsa-lib
    at-spi2-atk
    at-spi2-core
    cairo
    cups
    dbus
    expat
    gdk-pixbuf
    glib
    gtk3
    libdrm
    libgbm
    libnotify
    libcap_ng
    libsecret
    libseccomp
    libuuid
    libxkbcommon
    libxcb
    libx11
    libxcomposite
    libxdamage
    libxext
    libxfixes
    libxrandr
    libxtst
    nspr
    nss
    pango
    stdenv.cc.cc.lib
  ];

  # Loaded with dlopen at run time, so autoPatchelf cannot see them.
  runtimeDependencies = [
    systemd
    vulkan-loader
    libnotify
  ];

  dontConfigure = true;
  dontBuild = true;
  # Strip can damage the Electron binary.
  dontStrip = true;
  # wrapGAppsHook3 wraps with makeWrapper below.
  dontWrapGApps = true;

  unpackPhase = ''
    runHook preUnpack
    dpkg-deb --fsys-tarfile "$src" | tar -x --no-same-permissions --no-same-owner
    runHook postUnpack
  '';

  installPhase = ''
    runHook preInstall
    mkdir -p "$out"
    cp -r usr/. "$out/"
    if [ -d opt ]; then cp -r opt "$out/opt"; fi
    runHook postInstall
  '';

  preFixup = ''
    gappsWrapperArgs+=(
      --prefix PATH : ${lib.makeBinPath [ xdg-utils ]}
      # The chrome-sandbox helper cannot be setuid root in the Nix store.
      # --no-sandbox turns off the Chromium sandbox for this app.
      # The Ozone flags select Wayland when the session is Wayland, X11 otherwise.
      --add-flags "--no-sandbox --ozone-platform-hint=auto --enable-features=WaylandWindowDecorations"
    )
  '';

  postFixup = ''
    wrapProgram "$out/bin/claude-desktop" "''${gappsWrapperArgs[@]}"
  '';

  meta = {
    description = "Claude Desktop, the official Anthropic Linux build";
    homepage = "https://claude.ai/download";
    license = lib.licenses.unfree;
    sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
    platforms = builtins.attrNames sources;
    mainProgram = "claude-desktop";
  };
}
