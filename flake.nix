{
  description = "Nixos config flake";

  nixConfig = {
    extra-substituters = [
      "https://nix-community.cachix.org"
      "https://lucidph3nx-nixos-config.cachix.org"
    ];
    extra-trusted-public-keys = [
      "nix-community.cachix.org-1:mB9FSh9qf2dCimDSUo8Zy7bkq5CX+/rkCWyvRCYg3Fs="
      "lucidph3nx-nixos-config.cachix.org-1:gXiGMMDnozkXCjvOs9fOwKPZNIqf94ZA/YksjrKekHE="
    ];
  };

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    nixpkgs-stable.url = "github:nixos/nixpkgs/nixos-25.11";
    nixpkgs-master.url = "github:nixos/nixpkgs/master";
    disko = {
      url = "github:nix-community/disko";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    impermanence = {
      url = "github:nix-community/impermanence";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    sops-nix = {
      url = "github:mic92/sops-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    home-manager = {
      url = "github:nix-community/home-manager/master";
      inputs.nixpkgs.follows = "nixpkgs";
    };
    darwin = {
      url = "github:lnl7/nix-darwin";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      home-manager,
      darwin,
      ...
    }@inputs:
    let
      inherit (self) outputs;
      systems = [
        "x86_64-linux"
        "aarch64-darwin"
      ];
      forEachSystem = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      mkSystem =
        {
          system,
          configFile,
          extraModules ? [ ],
          extraConfig ? { },
        }:
        let
          lib = nixpkgs.lib;
        in
        nixpkgs.lib.nixosSystem {
          inherit system;
          pkgs = import nixpkgs {
            inherit system;
            config = lib.mkMerge [
              {
                allowUnfree = true;
                # See overlays/default.nix (bitwarden-desktop block) for
                # context + removal condition.
                permittedInsecurePackages = [ "electron-39.8.10" ];
              }
              extraConfig
            ];
            overlays = [ self.overlays.modifications ];
          };
          specialArgs = {
            inherit inputs outputs;
            isLinux = true;
          };
          modules = [
            ./machines/${configFile}/configuration.nix
            home-manager.nixosModules.default
            inputs.impermanence.nixosModules.impermanence
          ]
          ++ extraModules;
        };

      mkDarwinSystem =
        {
          system,
          configFile,
          extraModules ? [ ],
          extraConfig ? { },
        }:
        let
          lib = nixpkgs.lib;
        in
        darwin.lib.darwinSystem {
          inherit system;
          pkgs = import nixpkgs {
            inherit system;
            config = lib.mkMerge [
              {
                allowUnfree = true;
                allowUnsupportedSystem = true;
              }
              extraConfig
            ];
            overlays = [ self.overlays.modifications ];
          };
          specialArgs = {
            inherit inputs outputs;
            isLinux = false;
          };
          modules = [
            ./machines/${configFile}/configuration.nix
            home-manager.darwinModules.home-manager
            ./modules/darwin/impermanence-stub.nix
          ]
          ++ extraModules;
        };
    in
    {
      overlays = import ./overlays { inherit inputs outputs; };

      formatter = forEachSystem (pkgs: pkgs.nixfmt);

      nixosConfigurations = {
        navi = mkSystem {
          system = "x86_64-linux";
          configFile = "navi";
        };
        tui = mkSystem {
          system = "x86_64-linux";
          configFile = "tui";
        };
      };

      darwinConfigurations = {
        m4mac = mkDarwinSystem {
          system = "aarch64-darwin";
          configFile = "m4mac";
        };
      };

      packages = forEachSystem (pkgs: {
        # Default prism build — no Go test execution. Used by
        # nixosConfigurations / darwinConfigurations and by local
        # `nh switch` so rebuilds are fast. The Go test suite is owned
        # by the `go-tests` CI job (see .github/workflows/pr-gate.yml).
        # The homeless-shelter sandbox signal is preserved by the
        # `nix-build-prism-checked` CI job, which builds prism with
        # `runChecks = true` (see pkgs/prism.nix).
        prism = pkgs.callPackage ./pkgs/prism.nix { };

        # Default battery-monitor build — same `runChecks` split as
        # prism. Used by nixosConfigurations and local `nh switch`.
        # The `nix-build-battery-monitor-checked` CI job overrides
        # `runChecks = true` to preserve the homeless-shelter signal.
        battery-monitor = pkgs.callPackage ./pkgs/battery-monitor.nix { };

        # x/vt fidelity spike (issue #2141). Sibling Go module to prism;
        # has its own go.mod precisely so adding/removing it never forces
        # a prism `vendorHash` recompute. Hand-graded characterisation
        # tool, no test suite — see pkgs/mux-spike.nix.
        mux-spike = pkgs.callPackage ./pkgs/mux-spike.nix { };
      });

      devShells = forEachSystem (pkgs: {
        default = pkgs.mkShell {
          buildInputs = [
            pkgs.sops
          ];
        };
      });
    };
}
