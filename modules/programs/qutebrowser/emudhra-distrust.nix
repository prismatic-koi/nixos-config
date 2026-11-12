# QtWebEngine trusts the compromised eMudhra intermediates. This module marks
# each intermediate as distrusted (p,p,p) in the user NSS database
# ~/.pki/nssdb, which QtWebEngine reads on Linux. On macOS, QtWebEngine does
# not read this database, so default.nix imports this module on Linux only.
{
  config,
  pkgs,
  lib,
  ...
}:
let
  # Byte-identical copy of
  # https://emudhra.we-have-a-problem.com/impacted-intermediates.json. Use the
  # full set: the 8-hash Chromium blocklist subset does not block all of the
  # certificates.
  intermediatesFile = ./emudhra-impacted-intermediates.json;
  intermediatesSha256 = "f67b3abe9b4879f4e91874ed041beccd71257f1db41c588c724ccf12aa90c59e";
  intermediates =
    lib.throwIfNot (builtins.hashFile "sha256" intermediatesFile == intermediatesSha256)
      "emudhra-impacted-intermediates.json does not match the pinned sha256. Keep the file byte-identical to the upstream copy."
      (builtins.fromJSON (builtins.readFile intermediatesFile));

  certutil = "${pkgs.nssTools}/bin/certutil";

  # For a certificate that is already in the database, certutil -A sets the
  # trust again and adds no duplicate entry.
  addDistrust =
    cert:
    let
      sha256 = lib.toLower cert.sha256;
    in
    ''
      run ${certutil} -A -d "sql:$_qbNssdb" -t p,p,p \
        -n ${lib.escapeShellArg "eMudhra ${sha256}"} \
        -i ${pkgs.writeText "emudhra-${sha256}.pem" cert.pem}
    '';
in
{
  config = lib.mkIf config.nx.programs.qutebrowser.enable {
    # nixpkgs marks qutebrowser insecure because QtWebEngine accepts the
    # compromised eMudhra intermediates. The distrust entries below are the
    # mitigation.
    # - https://github.com/NixOS/nixpkgs/pull/584791
    # - https://github.com/NixOS/nixpkgs/issues/576544
    # - https://github.com/qutebrowser/qutebrowser/issues/9032
    # Remove this permit and this module when nixpkgs fixes qtwebengine or
    # removes the insecure flag from qutebrowser.
    nixpkgs.config.permittedInsecurePackages = [ "qutebrowser-3.7.0" ];

    home-manager.users.${config.nx.username} =
      { lib, ... }:
      {
        # The home-manager service runs activation at each boot. On an
        # impermanent host, this step adds the entries again after the wipe.
        home.activation.qutebrowserDistrustEmudhra = lib.hm.dag.entryAfter [ "writeBoundary" ] ''
          _qbNssdb="$HOME/.pki/nssdb"
          run mkdir -p -m 700 "$HOME/.pki" "$_qbNssdb"
          if [ ! -f "$_qbNssdb/cert9.db" ]; then
            run ${certutil} -N -d "sql:$_qbNssdb" --empty-password
          fi
          ${lib.concatMapStrings addDistrust intermediates}
        '';
      };
  };
}
