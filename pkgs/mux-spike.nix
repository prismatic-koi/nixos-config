{
  lib,
  buildGoModule,
}:

# The x/vt fidelity spike — see issue #2141 and
# modules/programs/prism/prism/docs/multiplexer-proposal.md.
#
# This package is an interactive characterisation tool, not production
# code. It is a sibling Go module to `prism` precisely so the spike has
# zero impact on `pkgs/prism.nix`'s `vendorHash` — adding or removing
# the spike never forces a prism rebuild.
#
# `doCheck` is hard-wired to false: the spike has no Go test suite (it
# is a feasibility experiment whose output is `report.md` + curated
# captures, not unit tests). No `runChecks` knob is exposed because
# there is nothing to gate on — symmetric with the AC's "no new CI lane"
# requirement.
buildGoModule {
  pname = "mux-spike";
  version = "0.1.0";

  src = ../modules/programs/prism/mux-spike;

  # Single binary at the module root.
  subPackages = [ "." ];

  doCheck = false;

  vendorHash = "sha256-7v14Aa0fNX9SgPp67cdsm+/ZWu5L5SwhoUafE20XDNM=";

  ldflags = [
    "-s"
    "-w"
  ];

  meta = {
    description = "x/vt fidelity spike for the prism-native multiplexer programme (issue #2141)";
    mainProgram = "mux-spike";
    license = lib.licenses.mit;
  };
}
