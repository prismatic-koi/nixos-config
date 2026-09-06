{
  lib,
  python3Packages,
  fetchFromGitHub,
  makeWrapper,
  kubernetes-helm,
  kustomize,
  fluxcd,
}:

# Upstream flux-local requires Python >= 3.13. When this pin is bumped and
# 3.13 support is dropped upstream, this version must move too.
python3Packages.buildPythonApplication rec {
  pname = "flux-local";
  version = "8.4.0";
  pyproject = true;

  src = fetchFromGitHub {
    owner = "allenporter";
    repo = "flux-local";
    rev = version;
    hash = "sha256-dKU06DyyVk7wjIRSG85/TvLRpi+L/zFimzjpi7Ajjlw=";
  };

  build-system = with python3Packages; [ setuptools ];

  dependencies = with python3Packages; [
    gitpython
    pyyaml
    aiofiles
    mashumaro
    nest-asyncio
    oras
    pytest
    pytest-asyncio
    python-slugify
  ];

  nativeBuildInputs = [ makeWrapper ];

  # Upstream tests need helm and network access, neither of which is
  # available in the Nix build sandbox.
  doCheck = false;

  pythonImportsCheck = [ "flux_local" ];

  postFixup = ''
    wrapProgram $out/bin/flux-local \
      --prefix PATH : ${
        lib.makeBinPath [
          kubernetes-helm
          kustomize
          fluxcd
        ]
      }
  '';

  meta = {
    description = "GitOps for Flux without a cluster";
    homepage = "https://github.com/allenporter/flux-local";
    license = lib.licenses.asl20;
    maintainers = [ ];
    mainProgram = "flux-local";
  };
}
