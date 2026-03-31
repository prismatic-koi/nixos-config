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
    impermanence.url = "github:nix-community/impermanence";
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
    pre-commit-hooks = {
      url = "github:cachix/pre-commit-hooks.nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      home-manager,
      darwin,
      pre-commit-hooks,
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
              { allowUnfree = true; }
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

      checks = forEachSystem (pkgs: {
        pre-commit-check = pre-commit-hooks.lib.${pkgs.stdenv.hostPlatform.system}.run {
          src = ./.;
          hooks = {
            nixfmt.enable = true;
            sops-check = {
              enable = true;
              name = "sops-check";
              description = "Check if secret files are encrypted";
              entry = "${pkgs.gnugrep}/bin/grep -q 'ENC\\[AES256_GCM' ";
              # Catch .sops, .sops.yaml, and any file in a secrets directory
              files = ".*\\.sops.*|.*/secrets/.*";
              pass_filenames = true;
            };
          };
        };
      });

      devShells = forEachSystem (pkgs: {
        default = pkgs.mkShell {
          inherit (self.checks.${pkgs.stdenv.hostPlatform.system}.pre-commit-check) shellHook;
          buildInputs = [
            pkgs.sops
            pkgs.gnugrep
            pkgs.nixfmt
          ];
        };
      });
    };
}
