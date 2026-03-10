{
  config,
  pkgs,
  lib,
  ...
}:
{
  options = {
    nx.programs.python.enable = lib.mkEnableOption "enables python env" // {
      default = true;
    };
  };
  config = lib.mkIf config.nx.programs.python.enable {
    home-manager.users.${config.nx.username} = {
      home.packages = with pkgs; [
        (python3.withPackages (python313Packages: [
          python313Packages.beautifulsoup4
          python313Packages.requests
          (python313Packages.buildPythonPackage rec {
            pname = "specify-cli";
            version = "unstable-2024-10-03";
            pyproject = true;
            src = fetchFromGitHub {
              owner = "github";
              repo = "spec-kit";
              rev = "e6d6f3cdee99752baee578896797400a72430ec0";
              sha256 = "sha256-A5WQ6/YeEfYrGRxO/V7grKB3O2wv4WIXBvNBAYxAx4Y=";
            };
            build-system = with python313Packages; [
              hatchling
            ];
            propagatedBuildInputs = with python313Packages; [
              httpx
              platformdirs
              readchar
              rich
              truststore
              typer
            ];
            doCheck = false;
          })
        ]))
      ];
    };
  };
}
