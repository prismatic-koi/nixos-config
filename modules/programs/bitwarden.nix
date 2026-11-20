# Workaround for bitwarden/clients#20703 / issue #1894:
# The `unlock-via-sdk` feature flag is server-pushed and triggers a broken code
# path in bitwarden-cli that causes `bw unlock --raw` to return a bogus session.
# Pinning to nixpkgs-stable (overlays/default.nix) is insufficient because the
# bug surfaces regardless of client version when the server sends the flag.
# This module patches data.json after every activation to keep the flag false.
# See also: modules/programs/qutebrowser/userscripts/bitwarden-prefetch
{
  config,
  pkgs,
  lib,
  ...
}:
let
  # api.bitwarden.com resolves to a Fastly us-eu POP map. From NZ the IPv4
  # TCP connect takes about 290 ms, which is more than the 250 ms default
  # happy-eyeballs attempt timeout of Node. Node then stops the connect and
  # `bw` fails with FetchError ETIMEDOUT. Keep happy eyeballs on, and give
  # each address attempt more time.
  bitwarden-cli-wrapped = pkgs.symlinkJoin {
    name = "bitwarden-cli-${pkgs.bitwarden-cli.version}";
    inherit (pkgs.bitwarden-cli) version meta;
    paths = [ pkgs.bitwarden-cli ];
    nativeBuildInputs = [ pkgs.makeWrapper ];
    postBuild = ''
      wrapProgram $out/bin/bw \
        --prefix NODE_OPTIONS " " "--network-family-autoselection-attempt-timeout=1000"
    '';
  };
in
{
  options = {
    nx.programs.bitwarden.enable = lib.mkEnableOption "enables bitwarden-desktop" // {
      default = true;
    };
    nx.programs.bitwarden.cliPackage = lib.mkOption {
      type = lib.types.package;
      readOnly = true;
      default = bitwarden-cli-wrapped;
      description = "The wrapped bitwarden-cli package. Use this, not pkgs.bitwarden-cli, for every `bw` call.";
    };
  };
  config = lib.mkIf config.nx.programs.bitwarden.enable {
    home-manager.users.${config.nx.username} =
      {
        lib,
        ...
      }:
      {
        home.packages = [
          pkgs.bitwarden-desktop
          bitwarden-cli-wrapped
        ];
        home.persistence."/persist" = {
          directories = [
            ".config/Bitwarden"
            ".config/Bitwarden CLI"
          ];
        };
        xdg.desktopEntries.bitwarden = lib.mkIf pkgs.stdenv.hostPlatform.isLinux {
          name = "Bitwarden";
          comment = "Secure and free password manager for all of your devices";
          icon = "bitwarden";
          # force wayland
          exec = "bitwarden --enable-features=UseOzonePlatform --ozone-platform=wayland";
          mimeType = [ "x-scheme-handler/bitwarden" ];
        };
        # Disable the server-pushed `unlock-via-sdk` feature flag after every
        # activation. When the flag is true, `bw unlock --raw` returns a session
        # that does not actually unlock the vault (bitwarden/clients#20703, #1894).
        # The hook is a no-op when data.json does not yet exist (pre-login), and
        # idempotent when the flag is already false.
        home.activation.bitwardenDisableUnlockViaSdk = lib.hm.dag.entryAfter [ "writeBoundary" ] ''
          _bw_data="$HOME/.config/Bitwarden CLI/data.json"
          if [ -f "$_bw_data" ]; then
            if ! ${pkgs.jq}/bin/jq -e \
                 '.global_config_byServer."https://api.bitwarden.com".featureStates."unlock-via-sdk" == false' \
                 "$_bw_data" >/dev/null 2>&1; then
              ${pkgs.jq}/bin/jq \
                '.global_config_byServer."https://api.bitwarden.com".featureStates."unlock-via-sdk" = false' \
                "$_bw_data" > "$_bw_data.tmp" \
                && mv "$_bw_data.tmp" "$_bw_data"
            fi
          fi
        '';
      };
  };
}
